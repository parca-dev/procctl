//go:build linux

package exclusive

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/parca-dev/procctl"
)

// End-to-end exclusive-resource tests with real C holders (examples/token).
// Overlap is judged only from the clients' own CLOCK_BOOTTIME logs.

type tokenEv struct {
	Ev    string `json:"ev"`
	PID   int    `json:"pid"`
	Grant uint64 `json:"grant"`
	TS    uint64 `json:"ts"`
}

type tokenProc struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	evs    []tokenEv
	exitTS uint64 // CLOCK_BOOTTIME when we killed it: the resource is freed by then
}

// kill SIGKILLs the process and records when, for the overlap check.
func (p *tokenProc) kill() {
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	_, _ = p.cmd.Process.Wait()
	p.mu.Lock()
	p.exitTS = bootNS()
	p.mu.Unlock()
}

func bootNS() uint64 {
	var ts syscall.Timespec
	_ = clockGettime(&ts)
	return uint64(ts.Nano())
}

func (p *tokenProc) events() []tokenEv {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tokenEv(nil), p.evs...)
}

func (p *tokenProc) count(ev string) int {
	n := 0
	for _, e := range p.events() {
		if e.Ev == ev {
			n++
		}
	}
	return n
}

func buildToken(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler")
	}
	out := filepath.Join(t.TempDir(), "token-client")
	cmd := exec.Command(cc, "-std=gnu11", "-O1", "-Wall", "-Werror", "-I../c", "-o", out,
		"../examples/token/client.c", "-lpthread")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cc: %v\n%s", err, b)
	}
	return out
}

func startToken(t *testing.T, bin string, args ...string) *tokenProc {
	t.Helper()
	p := &tokenProc{cmd: exec.Command(bin, args...)}
	out, _ := p.cmd.StdoutPipe()
	p.cmd.Stderr = os.Stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	go func() {
		s := bufio.NewScanner(out)
		once := sync.Once{}
		for s.Scan() {
			var e tokenEv
			if json.Unmarshal(s.Bytes(), &e) == nil {
				p.mu.Lock()
				p.evs = append(p.evs, e)
				p.mu.Unlock()
				once.Do(func() { close(ready) })
			}
		}
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	})
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("token client not ready")
	}
	return p
}

func startTokenAgent(t *testing.T, hold, deadline time.Duration) *Arbiter {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := procctl.NewRegistry(procctl.RegistryOptions{
		Consumer: "token", RescanInterval: 100 * time.Millisecond, Lease: 2 * time.Second, Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = reg.Run(ctx) }()
	arb := New(func() []Target {
		var ts []Target
		for _, c := range reg.Clients() {
			ts = append(ts, c)
		}
		return ts
	}, Options{
		Kind: 1, Policy: RoundRobin{Hold: hold, Deadline: deadline},
		Ready: reg.FirstScanDone(), Logger: quiet, BlockedAfter: 500 * time.Millisecond,
	})
	go func() { _ = arb.Run(ctx) }()
	t.Cleanup(func() { cancel(); _ = reg.Close() })
	return arb
}

// checkNoOverlap: a client "uses" the resource from its acquire event to its
// release event (or forever, if it never released and is still alive).
func checkNoOverlap(t *testing.T, procs ...*tokenProc) {
	t.Helper()
	type span struct {
		pid        int
		start, end uint64
	}
	var spans []span
	for _, p := range procs {
		var open *span
		for _, e := range p.events() {
			switch e.Ev {
			case "acquire":
				open = &span{pid: e.PID, start: e.TS, end: ^uint64(0)}
			case "release":
				if open != nil {
					open.end = e.TS
					spans = append(spans, *open)
					open = nil
				}
			}
		}
		if open != nil {
			p.mu.Lock()
			if p.exitTS != 0 {
				open.end = p.exitTS
			}
			p.mu.Unlock()
			spans = append(spans, *open)
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			t.Fatalf("OVERLAP: pid %d acquired at %d while pid %d held until %d",
				spans[i].pid, spans[i].start, spans[i-1].pid, spans[i-1].end)
		}
	}
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTokenRotation(t *testing.T) {
	bin := buildToken(t)
	ps := []*tokenProc{
		startToken(t, bin, "--acquire-ms", "50"),
		startToken(t, bin, "--acquire-ms", "50"),
		startToken(t, bin, "--acquire-ms", "50"),
	}
	startTokenAgent(t, 300*time.Millisecond, 2*time.Second)
	time.Sleep(4 * time.Second)
	checkNoOverlap(t, ps...)
	for _, p := range ps {
		if n := p.count("acquire"); n < 2 {
			t.Errorf("pid %d acquired %d times in 4s with 3 clients and 300ms turns", p.cmd.Process.Pid, n)
		}
	}
}

func TestTokenLoneHolderRenews(t *testing.T) {
	bin := buildToken(t)
	p := startToken(t, bin, "--acquire-ms", "50")
	arb := startTokenAgent(t, 200*time.Millisecond, time.Second)
	time.Sleep(3 * time.Second)
	if a, r := p.count("acquire"), p.count("release"); a != 1 || r != 0 {
		t.Fatalf("lone holder: %d acquires, %d releases; want 1, 0 (renewal by transfer)", a, r)
	}
	if s := arb.Snapshot(); len(s) != 1 || s[0].Renewals < 5 {
		t.Fatalf("expected many renewals: %+v", s)
	}
}

func TestTokenHolderKilled(t *testing.T) {
	bin := buildToken(t)
	a := startToken(t, bin, "--acquire-ms", "50")
	b := startToken(t, bin, "--acquire-ms", "50")
	startTokenAgent(t, 10*time.Second, 20*time.Second) // no rotation during the test
	waitFor(t, 3*time.Second, "a holder", func() bool { return a.count("acquire")+b.count("acquire") == 1 })
	holder, other := a, b
	if b.count("acquire") == 1 {
		holder, other = b, a
	}
	time.Sleep(200 * time.Millisecond)
	killed := time.Now()
	holder.kill()
	waitFor(t, 2*time.Second, "hand-over after holder exit", func() bool { return other.count("acquire") == 1 })
	t.Logf("hand-over after SIGKILL took %v", time.Since(killed).Round(time.Millisecond))
	checkNoOverlap(t, a, b)
}

func TestTokenHolderStoppedBlocks(t *testing.T) {
	bin := buildToken(t)
	a := startToken(t, bin, "--acquire-ms", "50")
	b := startToken(t, bin, "--acquire-ms", "50")
	arb := startTokenAgent(t, 200*time.Millisecond, 600*time.Millisecond)
	waitFor(t, 3*time.Second, "a holder", func() bool { return a.count("acquire")+b.count("acquire") >= 1 })
	// Stop whoever currently holds (most recent acquire without a release).
	holder, other := a, b
	if b.count("acquire") > b.count("release") {
		holder, other = b, a
	}
	_ = holder.cmd.Process.Signal(syscall.SIGSTOP)
	before := other.count("acquire")
	time.Sleep(2 * time.Second) // ≫ hold and grant duration
	if other.count("acquire") != before {
		t.Fatal("granted to another process while the holder was stopped")
	}
	if s := arb.Snapshot(); len(s) != 1 || s[0].BlockedFor == 0 {
		t.Fatalf("resource should be reported blocked: %+v", s)
	}
	holder.kill()
	waitFor(t, 2*time.Second, "grant after the stopped holder died", func() bool { return other.count("acquire") > before })
	checkNoOverlap(t, a, b)
}

func clockGettime(ts *syscall.Timespec) error {
	const clockBoottime = 7
	_, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockBoottime, uintptr(unsafe.Pointer(ts)), 0)
	if e != 0 {
		return e
	}
	return nil
}
