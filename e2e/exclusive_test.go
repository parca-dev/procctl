package e2e

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// bootNow is CLOCK_BOOTTIME, the clock of the clients' logs. On kind the node
// shares this host's kernel, so the test can timestamp its own actions.
func bootNow(t *testing.T) uint64 {
	t.Helper()
	if !onKind {
		t.Skip("needs the node's clock (kind)")
	}
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return uint64(ts.Nano())
}

// Exclusive timing for the fast suite: turns of hold, grants of deadline.
const (
	hold     = time.Second
	deadline = 3 * time.Second
)

func exclusiveAgentArgs() []string {
	return []string{"--exclusive", "--hold=" + hold.String(), "--deadline=" + deadline.String()}
}

// TestS14Exclusive acts on "the current holder": longer turns leave time to
// find it (by its log and the agent's summary) while it still holds.
const killHold = 2 * time.Second

var exclusiveClientArgs = []string{"--exclusive", "--acquire-ms=100"}

// interval is one may-hold period of one client, from its own log: from the
// "acquire" line (logged while ACQUIRING, before using the resource) to the
// "release" line (logged before reporting RELEASED).
type interval struct {
	pod        string
	start, end uint64 // end = MaxUint64: still holding
}

// holdIntervals reads every client's log (including a killed container's)
// and builds its intervals. An interval left open by a killed process ends at
// the kill time recorded for that pod.
func (e *scenario) holdIntervals(pods []string, killedAt map[string]uint64) []interval {
	e.t.Helper()
	var out []interval
	for _, p := range pods {
		evs := e.allEvents(p)
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS < evs[j].TS })
		open := uint64(0)
		for _, ev := range evs {
			switch ev.Ev {
			case "acquire":
				open = ev.TS
			case "release":
				if open != 0 {
					out = append(out, interval{p, open, ev.TS})
					open = 0
				}
			case "start": // a new container: the previous one died holding
				if open != 0 {
					end, ok := killedAt[p]
					if !ok || end < open {
						end = ev.TS
					}
					out = append(out, interval{p, open, end})
					open = 0
				}
			}
		}
		if open != 0 {
			out = append(out, interval{p, open, math.MaxUint64})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// checkNoOverlap is the S14 invariant: holding intervals never overlap.
func checkNoOverlap(t *testing.T, ivs []interval) {
	t.Helper()
	bad := 0
	for i := 1; i < len(ivs); i++ {
		if prev, cur := ivs[i-1], ivs[i]; cur.start < prev.end {
			bad++
			t.Errorf("OVERLAP: %s acquired at %d while %s held [%d, %s)", cur.pod, cur.start, prev.pod, prev.start, endStr(prev.end))
		}
	}
	if bad == 0 {
		t.Logf("%d holding intervals, none overlapping", len(ivs))
	}
}

func endStr(e uint64) string {
	if e == math.MaxUint64 {
		return "still holding"
	}
	return strconv.FormatUint(e, 10)
}

// acquiresAfter returns the first acquire by any pod other than skip after ts.
func (e *scenario) acquiresAfter(pods []string, skip string, ts uint64) (string, uint64, bool) {
	e.t.Helper()
	best, who := uint64(math.MaxUint64), ""
	for _, p := range pods {
		if p == skip {
			continue
		}
		for _, ev := range e.allEvents(p) {
			if ev.Ev == "acquire" && ev.TS > ts && ev.TS < best {
				best, who = ev.TS, p
			}
		}
	}
	return who, best, who != ""
}

func (e *scenario) waitAcquireAfter(pods []string, skip string, ts uint64, timeout time.Duration, what string) (string, uint64) {
	e.t.Helper()
	until := time.Now().Add(timeout)
	for time.Now().Before(until) {
		if who, at, ok := e.acquiresAfter(pods, skip, ts); ok {
			return who, at
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("no acquire within %v: %s", timeout, what)
	return "", 0
}

func (e *scenario) acquires(pod string) int {
	n := 0
	for _, ev := range e.allEvents(pod) {
		if ev.Ev == "acquire" {
			n++
		}
	}
	return n
}

// holdingNow: by its own log, pod acquired less than half a turn ago and
// hasn't released since (the agent's summary can be half a second old).
func (e *scenario) holdingNow(pod string, now uint64, turn time.Duration) bool {
	e.t.Helper()
	var last clientEvent
	for _, ev := range e.allEvents(pod) {
		if ev.Ev == "acquire" || ev.Ev == "release" {
			last = ev
		}
	}
	return last.Ev == "acquire" && now-last.TS < uint64(turn/2)
}

// waitHolder returns a client that is holding right now, by its own log,
// early enough in its turn to act on it, and whose process (per the summary)
// is still alive as that process.
func (e *scenario) waitHolder(cl []string, turn time.Duration) (string, summary) {
	e.t.Helper()
	until := time.Now().Add(30 * time.Second)
	for time.Now().Before(until) {
		if s, ok := e.latest(); ok && allAttached(cl)(s) {
			for _, c := range s.Clients {
				if c.Holding && e.holdingNow(c.Pod, bootNow(e.t), turn) && sameProcess(s.Node, c.HostPID, c.HostStart) {
					return c.Pod, s
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatal("no holder early in its turn")
	return "", summary{}
}

// heldAt reports whether pod, by its own log, held at ts: its last acquire
// before ts has no release before ts. Used to confirm that a client signalled
// as "the holder" really held when the signal landed (it may have released
// while the test was looking it up).
func (e *scenario) heldAt(pod string, ts uint64) bool {
	e.t.Helper()
	var last clientEvent
	for _, ev := range e.allEvents(pod) {
		if (ev.Ev == "acquire" || ev.Ev == "release") && ev.TS <= ts && ev.TS >= last.TS {
			last = ev
		}
	}
	return last.Ev == "acquire"
}

func ms(d uint64) string { return fmt.Sprintf("%.0f ms", float64(d)/1e6) }

// S14: exclusive resources under round-robin, holder failures and agent
// failures. The invariant, judged from the clients' own logs: holding
// intervals never overlap.
func TestS14Exclusive(t *testing.T) {
	t.Parallel()
	e := newScenario(t, "s14")
	e.agent(agentDS, nil, "--exclusive", "--hold="+killHold.String(), "--deadline="+(killHold+deadline).String())
	cl := e.clients(clientDP, 3, exclusiveClientArgs...)
	s := e.wait(30*time.Second, "attached", allAttached(cl))
	node := s.Node
	killed := map[string]uint64{}

	// Round-robin.
	time.Sleep(killHold * time.Duration(len(cl)+1))
	for _, p := range cl {
		if e.acquires(p) == 0 {
			t.Errorf("round-robin: %s never acquired", p)
		}
	}

	// Holder SIGKILLed mid-grant: re-granted right after its pidfd exit.
	// (If it released before the signal landed, the measurement is of a
	// normal turn: try again with the next holder.)
	var hp, who string
	var at uint64
	for try := 0; ; try++ {
		var s summary
		hp, s = e.waitHolder(cl, killHold)
		killed[hp] = bootNow(t) // before the signal: an acquire earlier than this is a real overlap
		signalClient(t, s, hp, "KILL")
		who, at = e.waitAcquireAfter(cl, hp, killed[hp], 10*time.Second, "after the holder was killed")
		kubectl(t, "-n", e.clientNS, "rollout", "status", "deployment/"+clientDP, "--timeout=60s")
		if e.heldAt(hp, killed[hp]) {
			break
		}
		if try == 4 {
			t.Fatal("never caught a holder before it released")
		}
		t.Logf("%s had released before the kill landed; again", hp)
		e.wait(60*time.Second, "restarted client attached", allAttached(e.pods(e.clientNS, "app="+clientDP)))
	}
	t.Logf("holder %s killed: %s acquired %s later", hp, who, ms(at-killed[hp]))
	if at-killed[hp] > uint64(3*time.Second) {
		t.Errorf("re-grant after a killed holder took %s", ms(at-killed[hp]))
	}
	e.wait(60*time.Second, "restarted client attached", allAttached(cl))

	// Holder SIGSTOPped: no re-grant while it lives; blocked time reported.
	var stopped uint64
	for try := 0; ; try++ {
		var s summary
		hp, s = e.waitHolder(cl, killHold)
		signalClient(t, s, hp, "STOP")
		stopped = bootNow(t)
		if e.heldAt(hp, stopped) {
			break
		}
		signal(t, node, "CONT", byPod(s)[hp].HostPID)
		if try == 4 {
			t.Fatal("never caught a holder before it released")
		}
		t.Logf("%s had released before the stop landed; again", hp)
	}
	time.Sleep(killHold + deadline + time.Second)
	if who, at, ok := e.acquiresAfter(cl, hp, stopped); ok {
		t.Errorf("%s acquired %s after holder %s was stopped", who, ms(at-stopped), hp)
	}
	s, _ = e.latest()
	if len(s.Resources) == 0 || s.Resources[0].BlockedMS < float64(deadline.Milliseconds()) {
		t.Errorf("stopped holder: blocked time not reported: %+v", s.Resources)
	} else {
		t.Logf("holder %s stopped: no re-grant for %v, blocked %.0f ms reported", hp, killHold+deadline+time.Second, s.Resources[0].BlockedMS)
	}
	signal(t, node, "CONT", byPod(s)[hp].HostPID)
	resumed := bootNow(t)
	who, at = e.waitAcquireAfter(cl, hp, resumed, 15*time.Second, "after the stopped holder resumed")
	t.Logf("holder resumed (deadline passed, self-released): %s acquired %s later", who, ms(at-resumed))

	// Agent SIGKILLed (mid-grant or mid-hand-off), then a rolling update.
	kills := 1
	if long {
		kills = 3
	}
	for i := 0; i < kills; i++ {
		prev, _ := e.latest()
		signal(t, node, "KILL", prev.AgentPID)
		e.wait(60*time.Second, "agent back", func(s summary) bool {
			return s.AgentPID != prev.AgentPID && allAttached(cl)(s)
		})
		time.Sleep(time.Duration(1300+700*i) * time.Millisecond)
	}
	before := bootNow(t)
	kubectl(t, "-n", e.agentNS, "rollout", "restart", "daemonset/"+agentDS)
	kubectl(t, "-n", e.agentNS, "rollout", "status", "daemonset/"+agentDS, "--timeout=120s")
	who, _ = e.waitAcquireAfter(cl, "", before, 30*time.Second, "after agent kills and a rolling update")
	t.Logf("after %d agent kill(s) and a rolling update: rotation continues (%s acquired)", kills, who)
	time.Sleep(2 * killHold)

	checkNoOverlap(t, e.holdIntervals(cl, killed))
}

// S14: rotation among many clients (long mode: 8 and 50 per resource).
func TestS14Many(t *testing.T) {
	if !long {
		t.Skip("E2E_LONG only")
	}
	t.Parallel()
	for _, n := range []int{8, 50} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			e := newScenario(t, fmt.Sprintf("s14-many%d", n))
			e.agent(agentDS, nil, exclusiveAgentArgs()...)
			cl := e.clients(clientDP, n, exclusiveClientArgs...)
			e.wait(90*time.Second, "attached", allAttached(cl))
			time.Sleep(time.Duration(n+2) * (hold + 500*time.Millisecond))
			for _, p := range cl {
				if e.acquires(p) == 0 {
					t.Errorf("%s never acquired", p)
				}
			}
			checkNoOverlap(t, e.holdIntervals(cl, nil))
		})
	}
}

// S14: a holder whose control thread hangs keeps the resource for as long as
// it lives (the arbiter never guesses); killing it frees the resource.
func TestS14HungHolder(t *testing.T) {
	t.Parallel()
	e := newScenario(t, "s14-hung")
	e.agent(agentDS, nil, exclusiveAgentArgs()...)
	hung := e.clients("hang", 1, append([]string{"--holder-hang"}, exclusiveClientArgs...)...)
	cl := e.clients(clientDP, 2, exclusiveClientArgs...)
	until := time.Now().Add(60 * time.Second)
	var hangAt uint64
	for hangAt == 0 {
		for _, ev := range e.events(hung[0]) {
			if ev.Ev == "hang" {
				hangAt = ev.TS
			}
		}
		if time.Now().After(until) {
			t.Fatal("the hanging holder never got the resource")
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2*deadline + time.Second)
	if who, at, ok := e.acquiresAfter(cl, "", hangAt); ok {
		t.Errorf("%s acquired %s after the holder hung", who, ms(at-hangAt))
	}
	s, _ := e.latest()
	if len(s.Resources) == 0 || s.Resources[0].BlockedMS < float64((2*deadline).Milliseconds()) {
		t.Errorf("hung holder: blocked time not reported: %+v", s.Resources)
	} else {
		t.Logf("hung holder: no re-grant, blocked %.0f ms reported", s.Resources[0].BlockedMS)
	}
	killed := map[string]uint64{hung[0]: bootNow(t)} // before the signal (see TestS14Exclusive)
	signalClient(t, s, hung[0], "KILL")
	kubectl(t, "-n", e.clientNS, "scale", "deployment/hang", "--replicas=0")
	who, at := e.waitAcquireAfter(cl, "", killed[hung[0]], 20*time.Second, "after the hung holder was killed")
	t.Logf("hung holder killed: %s acquired %s later", who, ms(at-killed[hung[0]]))
	ivs := e.holdIntervals(cl, nil)
	ivs = append(ivs, interval{hung[0], hangAt, killed[hung[0]]})
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].start < ivs[j].start })
	checkNoOverlap(t, ivs)
}

// S14: a lone holder is renewed, never released (E2E_LONE_HOLDER; default
// 8 s, 10 min long).
func TestS14LoneHolder(t *testing.T) {
	t.Parallel()
	d := 8 * time.Second
	if long {
		d = 10 * time.Minute
	}
	if v, err := time.ParseDuration(getenv("E2E_LONE_HOLDER", "")); err == nil {
		d = v
	}
	e := newScenario(t, "s14-lone")
	e.agent(agentDS, nil, exclusiveAgentArgs()...)
	cl := e.clients(clientDP, 1, exclusiveClientArgs...)
	e.wait(30*time.Second, "holding", func(s summary) bool {
		for _, c := range s.Clients {
			if c.Holding {
				return true
			}
		}
		return false
	})
	time.Sleep(d)
	acq, rel := 0, 0
	for _, ev := range e.events(cl[0]) {
		switch ev.Ev {
		case "acquire":
			acq++
		case "release":
			rel++
		}
	}
	s, _ := e.latest()
	if acq != 1 || rel != 0 {
		t.Errorf("lone holder over %v: %d acquires, %d releases; want 1 and 0", d, acq, rel)
	}
	if len(s.Resources) == 0 || s.Resources[0].Renewals == 0 {
		t.Errorf("no renewals: %+v", s.Resources)
	} else {
		t.Logf("lone holder over %v: 1 acquire, 0 releases, %d renewals", d, s.Resources[0].Renewals)
	}
}

// S16: wake latency. The agent rewrites the grant table; the client's control
// thread, asleep in a futex wait on the read-only mapping, wakes and reports
// grants_seen_gen in its state block, which the agent polls.
func TestS16WakeLatency(t *testing.T) {
	t.Parallel()
	e := newScenario(t, "s16")
	e.agent(agentDS, nil, "--wake-probe=50ms")
	cl := e.clients(clientDP, 4, "--exclusive")
	s := e.wait(60*time.Second, "100 wake samples per client", func(s summary) bool {
		if !allAttached(cl)(s) {
			return false
		}
		for _, c := range s.Clients {
			if c.WakeSamples < 100 {
				return false
			}
		}
		return true
	})
	var p50, p99 []float64
	for _, c := range s.Clients {
		p50 = append(p50, c.WakeP50us)
		p99 = append(p99, c.WakeP99us)
		if c.WakeFails > 0 {
			t.Errorf("%s: %d probes never seen", c.Pod, c.WakeFails)
		}
	}
	sort.Float64s(p50)
	sort.Float64s(p99)
	t.Logf("wake → grants_seen_gen: p50 median %.0f µs, worst p99 %.0f µs (%d clients)", p50[len(p50)/2], p99[len(p99)-1], len(p50))
	// Without a working futex wake on the read-only mapping, the thread only
	// notices at its 50 ms wait timeout.
	if p50[len(p50)/2] > 10000 {
		t.Errorf("median p50 %.0f µs: the futex wake isn't working", p50[len(p50)/2])
	}
	if p99[len(p99)-1] > 25000 {
		t.Errorf("worst p99 %.0f µs (target < 10 ms)", p99[len(p99)-1])
	}
}
