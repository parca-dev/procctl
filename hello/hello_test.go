//go:build linux

package hello

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Needs CAP_BPF + CAP_PERFMON (run as root):
//
//	go test -c -o /tmp/hello.test ./hello && sudo /tmp/hello.test -test.v
func requireRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (BPF, uprobes, perf events)")
	}
}

func buildClient(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler")
	}
	out := filepath.Join(t.TempDir(), "procctl-testclient")
	cmd := exec.Command(cc, "-std=gnu11", "-O1", "-I../c", "-I../testclient", "-o", out,
		"../testclient/testclient.c", "-lpthread")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cc: %v\n%s", err, b)
	}
	return out
}

type notes struct {
	mu   sync.Mutex
	pids map[int][]time.Time
}

func (n *notes) notify(pid int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pids[pid] = append(n.pids[pid], time.Now())
}

func (n *notes) count0(pid int) int { return len(n.pids[pid]) }

func (n *notes) count(pid int) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.pids[pid])
}

func TestHelloFiresForNewProcess(t *testing.T) {
	requireRoot(t)
	bin := buildClient(t)
	n := &notes{pids: map[int][]time.Time{}}
	w, err := New(Options{Notify: n.notify, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond) // initial sweep

	// A never-seen binary whose init runs 500 ms after exec: the mapping event
	// must attach the uprobe in time for the hello probe to fire.
	cmd := exec.Command(bin, "--init-delay-ms", "500", "--exit-after-s", "3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	pid := cmd.Process.Pid
	deadline := time.Now().Add(3 * time.Second)
	for n.count(pid) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no mapping notice for pid %d; stats %+v", pid, w.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := w.Stats(); st.Uprobes == 0 && st.UprobeErrors > 0 {
		t.Skipf("mapping notice ok; uprobes unavailable (CAP_SYS_ADMIN?): %+v", st)
	}
	for w.Stats().Firings == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("hello never fired; stats %+v", w.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n.count(pid) < 2 { // once on the mapping event, once on the firing
		n.mu.Lock()
		t.Errorf("pid %d notified %d times; want the mapping notice and the hello firing (notified: %v)", pid, n.count0(pid), n.pids)
		n.mu.Unlock()
	}
	st := w.Stats()
	t.Logf("stats: %+v", st)
	if st.FilesWithHello == 0 || st.Uprobes == 0 || st.MappingEvents == 0 {
		t.Errorf("unexpected stats %+v", st)
	}

	// Once the client is gone, gc detaches the uprobes on its binary.
	cmd.Process.Kill()
	cmd.Wait()
	w.gc()
	after := w.Stats()
	if after.UprobesActive >= st.UprobesActive {
		t.Errorf("gc detached nothing: before %+v, after %+v", st, after)
	}
	t.Logf("after gc: %d uprobes active (was %d), %d files cached (was %d)",
		after.UprobesActive, st.UprobesActive, after.FilesCached, st.FilesCached)
}
