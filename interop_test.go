//go:build linux

package procctl

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// buildC compiles a C program against c/procctl.h, or skips the test.
func buildC(t *testing.T, src string) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler")
	}
	out := filepath.Join(t.TempDir(), filepath.Base(strings.TrimSuffix(src, ".c")))
	cmd := exec.Command(cc, "-std=gnu11", "-O1", "-Wall", "-Werror", "-Ic", "-o", out, src, "-lpthread")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cc %s: %v\n%s", src, err, b)
	}
	return out
}

// proc is a running C test process with captured stdout lines.
type proc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	mu    sync.Mutex
	lines []string
	ready chan struct{}
}

func startProc(t *testing.T, bin string, args ...string) *proc {
	t.Helper()
	p := &proc{cmd: exec.Command(bin, args...), ready: make(chan struct{})}
	p.stdin, _ = p.cmd.StdinPipe()
	out, _ := p.cmd.StdoutPipe()
	p.cmd.Stderr = os.Stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		s := bufio.NewScanner(out)
		once := sync.Once{}
		for s.Scan() {
			p.mu.Lock()
			p.lines = append(p.lines, s.Text())
			p.mu.Unlock()
			once.Do(func() { close(p.ready) })
		}
	}()
	t.Cleanup(func() {
		p.stdin.Close()
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	})
	select {
	case <-p.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("process never became ready")
	}
	return p
}

func (p *proc) waitLine(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for _, l := range p.lines {
			if strings.Contains(l, substr) {
				p.mu.Unlock()
				return
			}
		}
		p.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("no line containing %q; got:\n%s", substr, strings.Join(p.lines, "\n"))
}

func (p *proc) linesContaining(substr string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, l := range p.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

type agent struct {
	reg    *Registry
	cancel context.CancelFunc
	done   chan struct{}
}

func startAgent(t *testing.T, consumer string, lease time.Duration) *agent {
	t.Helper()
	reg, err := NewRegistry(RegistryOptions{Consumer: consumer, RescanInterval: 100 * time.Millisecond, Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &agent{reg: reg, cancel: cancel, done: make(chan struct{})}
	go func() { _ = reg.Run(ctx); close(a.done) }()
	<-reg.FirstScanDone()
	return a
}

// stop cancels Run without releasing ownership (a crash or a hang).
func (a *agent) stop() { a.cancel(); <-a.done }

func clientFor(t *testing.T, reg *Registry, pid int) *Client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range reg.Clients() {
			if c.Info().HostPID == pid {
				return c
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d never attached", pid)
	return nil
}

func TestEchoRoundTrip(t *testing.T) {
	bin := buildC(t, "examples/echo/client.c")
	p := startProc(t, bin, "--interval-ms", "5")
	a := startAgent(t, "echo", 2*time.Second)
	defer a.reg.Close()
	c := clientFor(t, a.reg, p.cmd.Process.Pid)
	if !c.Owned() {
		t.Fatal("not owner")
	}
	if c.Info().AttachPath != "pidfd_getfd" {
		t.Errorf("attach path %s", c.Info().AttachPath)
	}
	for i := 0; i < 20; i++ {
		gen, _, err := c.Write([]byte("hello"), nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := c.WaitAck(ctx, gen); err != nil {
			t.Fatalf("gen %d: %v", gen, err)
		}
		cancel()
		st, err := c.ReadState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Stale || !strings.HasPrefix(string(st.Consumer), "applied gen=") {
			t.Fatalf("state %+v %q", st, st.Consumer)
		}
	}
	p.waitLine(t, "config gen=40: hello", 2*time.Second)
	// A nil payload clears the configuration.
	if _, _, err := c.Write(nil, nil); err != nil {
		t.Fatal(err)
	}
	p.waitLine(t, "none (agent cleared config)", 2*time.Second)
}

func TestUninstallRevertsImmediately(t *testing.T) {
	bin := buildC(t, "examples/echo/client.c")
	p := startProc(t, bin, "--interval-ms", "5")
	a := startAgent(t, "echo", 10*time.Second)
	clientFor(t, a.reg, p.cmd.Process.Pid)
	p.waitLine(t, "agent lease active", 2*time.Second)
	before := p.linesContaining("using defaults")
	a.reg.Close(ReleaseLeases(true))
	a.stop()
	deadline := time.Now().Add(time.Second) // far below the 10 s lease
	for p.linesContaining("using defaults") == before {
		if time.Now().After(deadline) {
			t.Fatal("client did not revert after uninstall")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReleasedHandoverKeepsConfig(t *testing.T) {
	bin := buildC(t, "examples/echo/client.c")
	p := startProc(t, bin, "--interval-ms", "5")
	a1 := startAgent(t, "echo", 2*time.Second)
	c1 := clientFor(t, a1.reg, p.cmd.Process.Pid)
	gen, _, err := c1.Write([]byte("v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c1.WaitAck(ctx, gen); err != nil {
		t.Fatal(err)
	}
	expiredBefore := p.linesContaining("using defaults")
	a1.reg.Close() // RELEASED: a successor may claim immediately
	a1.stop()

	a2 := startAgent(t, "echo", 2*time.Second)
	defer a2.reg.Close()
	c2 := clientFor(t, a2.reg, p.cmd.Process.Pid)
	if !c2.Owned() {
		t.Fatal("successor did not claim a RELEASED region")
	}
	gen2, _, err := c2.Write([]byte("v2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gen2 <= gen {
		t.Fatalf("generation went backwards: %d after %d", gen2, gen)
	}
	if err := c2.WaitAck(ctx, gen2); err != nil {
		t.Fatal(err)
	}
	if n := p.linesContaining("using defaults"); n != expiredBefore {
		t.Fatalf("client reverted during the hand-over")
	}
}

func TestTwoAgentsOneOwner(t *testing.T) {
	bin := buildC(t, "examples/echo/client.c")
	p := startProc(t, bin, "--interval-ms", "5")
	pid := p.cmd.Process.Pid
	a1 := startAgent(t, "echo", 400*time.Millisecond)
	defer a1.reg.Close()
	c1 := clientFor(t, a1.reg, pid)
	a2 := startAgent(t, "echo", 400*time.Millisecond)
	defer a2.reg.Close()
	c2 := clientFor(t, a2.reg, pid)

	// Several lease periods with both healthy: exactly one owner, no flapping.
	for i := 0; i < 15; i++ {
		if c1.Owned() == c2.Owned() {
			t.Fatalf("owners: a1=%v a2=%v", c1.Owned(), c2.Owned())
		}
		if !c1.Owned() {
			t.Fatalf("ownership moved to the second agent at step %d", i)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, _, err := c2.Write([]byte("x"), nil); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("passive agent write: %v", err)
	}

	// The owner hangs (no heartbeats): the other claims after one lease (rule 4).
	a1.stop()
	deadline := time.Now().Add(3 * time.Second)
	for !c2.Owned() {
		if time.Now().After(deadline) {
			t.Fatal("second agent never claimed after the lease")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, err := c1.Write([]byte("stale owner"), nil); !errors.Is(err, ErrPreempted) && !errors.Is(err, ErrNotOwner) {
		t.Fatalf("former owner could still write: %v", err)
	}
}

func TestForkChildStateIsStale(t *testing.T) {
	bin := buildC(t, "examples/echo/client.c")
	p := startProc(t, bin, "--interval-ms", "5", "--fork-after-ms", "100")
	p.waitLine(t, "forked child pid=", 2*time.Second)
	p.waitLine(t, "state writes refused (-ECHILD)", 2*time.Second)
	var child int
	p.mu.Lock()
	for _, l := range p.lines {
		if i := strings.Index(l, "forked child pid="); i >= 0 {
			_, _ = fmtSscan(l[i+len("forked child pid="):], &child)
		}
	}
	p.mu.Unlock()
	t.Cleanup(func() { _ = unix.Kill(child, unix.SIGKILL) })

	id, _ := NewIdentity()
	cands, _, err := Scan(context.Background(), ScanOptions{Consumer: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	var parentFD, childFD = -1, -1
	for _, c := range cands {
		switch c.HostPID {
		case p.cmd.Process.Pid:
			parentFD = c.FD
		case child:
			childFD = c.FD
		}
	}
	if parentFD < 0 || childFD < 0 {
		t.Fatalf("expected the region in both parent and child: %+v", cands)
	}
	pc, err := Attach(p.cmd.Process.Pid, parentFD, AttachOptions{Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	cc, err := Attach(child, childFD, AttachOptions{Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if pc.Info().Key != cc.Info().Key {
		t.Fatal("parent and child should share one region")
	}
	ps, err := pc.ReadState(context.Background())
	if err != nil || ps.Stale {
		t.Fatalf("parent state: %+v %v", ps, err)
	}
	cs, err := cc.ReadState(context.Background())
	if err != nil || !cs.Stale {
		t.Fatalf("child state should be stale: %+v %v", cs, err)
	}
}

func TestForkReinitGetsOwnRegion(t *testing.T) {
	bin := buildC(t, "examples/echo/client.c")
	p := startProc(t, bin, "--interval-ms", "5", "--fork-after-ms", "100", "--reinit")
	p.waitLine(t, "reinit -> 0", 2*time.Second)
	a := startAgent(t, "echo", 2*time.Second)
	defer a.reg.Close()
	// The reinit child is ours if its parent is the process we started.
	ours := func(pid int) bool {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		return len(f) > 1 && f[1] == strconv.Itoa(p.cmd.Process.Pid)
	}
	countOurs := func() int {
		n := 0
		for _, c := range a.reg.Clients() {
			if pid := c.Info().HostPID; pid == p.cmd.Process.Pid || ours(pid) {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(3 * time.Second)
	for countOurs() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("want 2 regions (parent + reinit child), have %d", countOurs())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, c := range a.reg.Clients() {
		if c.Info().HostPID != p.cmd.Process.Pid && !ours(c.Info().HostPID) {
			continue // an unrelated echo client on this host
		}
		st, err := c.ReadState(context.Background())
		if err != nil || st.Stale {
			t.Fatalf("pid %d: state %+v err %v", c.Info().HostPID, st, err)
		}
	}
}

func TestGrantWakesControlThread(t *testing.T) {
	bin := buildC(t, "c/tests/wait_client.c")
	p := startProc(t, bin)
	a := startAgent(t, "waittest", 2*time.Second)
	defer a.reg.Close()
	c := clientFor(t, a.reg, p.cmd.Process.Pid)
	var id [16]byte
	for i := range id {
		id[i] = 0x42
	}
	ctx := context.Background()

	waitFor := func(pred func(State) bool) (State, time.Duration) {
		t0 := time.Now()
		for time.Since(t0) < 2*time.Second {
			st, err := c.ReadState(ctx)
			if err == nil && pred(st) {
				return st, time.Since(t0)
			}
			time.Sleep(200 * time.Microsecond)
		}
		t.Fatal("state predicate never held")
		return State{}, 0
	}
	entry := func(st State) ResourceEntry {
		for _, r := range st.Resources {
			if r.Kind == 1 && r.ID == id {
				return r
			}
		}
		return ResourceEntry{}
	}

	gen, grants, err := c.Write(nil, []Grant{{Kind: 1, ID: id, Duration: uint64(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if grants[0].GrantID != gen {
		t.Fatalf("new grant id %d != generation %d", grants[0].GrantID, gen)
	}
	st, took := waitFor(func(st State) bool { e := entry(st); return e.State == ResHolding && e.GrantID == gen })
	t.Logf("grant → HOLDING in %v (futex wake; the waiter's timeout is 250 ms)", took)
	if took > 200*time.Millisecond {
		t.Errorf("control thread was not woken promptly: %v", took)
	}
	if st.GrantsSeenGen < gen || entry(st).Flags&ResFlagWant == 0 {
		t.Fatalf("state %+v", st)
	}

	// Revoke: an empty grant table releases.
	rgen, _, err := c.Write(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, took = waitFor(func(st State) bool {
		e := entry(st)
		return e.State == ResReleased && e.GrantID == gen && st.GrantsSeenGen >= rgen
	})
	t.Logf("revoke → RELEASED in %v", took)
}

// fmtSscan parses a leading decimal integer.
func fmtSscan(s string, v *int) (int, error) {
	n := 0
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if i == 0 {
		return 0, errors.New("no number")
	}
	*v = n
	return 1, nil
}
