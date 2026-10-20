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
func holdIntervals(t *testing.T, pods []string, killedAt map[string]uint64) []interval {
	t.Helper()
	var out []interval
	for _, p := range pods {
		evs := allClientEvents(t, p)
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS < evs[j].TS })
		open := uint64(0)
		for _, e := range evs {
			switch e.Ev {
			case "acquire":
				open = e.TS
			case "release":
				if open != 0 {
					out = append(out, interval{p, open, e.TS})
					open = 0
				}
			case "start": // a new container: the previous one died holding
				if open != 0 {
					end, ok := killedAt[p]
					if !ok || end < open {
						end = e.TS
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
func acquiresAfter(t *testing.T, pods []string, skip string, ts uint64) (string, uint64, bool) {
	t.Helper()
	best, who := uint64(math.MaxUint64), ""
	for _, p := range pods {
		if p == skip {
			continue
		}
		for _, e := range allClientEvents(t, p) {
			if e.Ev == "acquire" && e.TS > ts && e.TS < best {
				best, who = e.TS, p
			}
		}
	}
	return who, best, who != ""
}

func waitAcquireAfter(t *testing.T, pods []string, skip string, ts uint64, timeout time.Duration, what string) (string, uint64) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if who, at, ok := acquiresAfter(t, pods, skip, ts); ok {
			return who, at
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("no acquire within %v: %s", timeout, what)
	return "", 0
}

func holder(s summary) (string, int) {
	for _, c := range s.Clients {
		if c.Holding {
			return c.Pod, c.HostPID
		}
	}
	return "", 0
}

// liveHolder is holder(s), but only if that process still exists on the node
// as the same process (start time): a summary can list a just-killed one.
func liveHolder(t *testing.T, s summary) (string, int) {
	t.Helper()
	for _, c := range s.Clients {
		if c.Holding && sameProcess(s.Node, c.HostPID, c.HostStart) {
			return c.Pod, c.HostPID
		}
	}
	return "", 0
}

// holdingNow: by its own log, pod acquired at least minHeld ago and hasn't
// released since (the agent's summary can be a second old).
func holdingNow(t *testing.T, pod string, now uint64) bool {
	t.Helper()
	var last clientEvent
	for _, e := range allClientEvents(t, pod) {
		if e.Ev == "acquire" || e.Ev == "release" {
			last = e
		}
	}
	return last.Ev == "acquire" && now-last.TS < uint64(2*time.Second)
}

// waitHolder returns a client that is holding right now, by its own log,
// early enough in its turn to act on it.
func waitHolder(t *testing.T, cl []string) (string, int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := latestSummary(t); ok && allAttached(t, cl)(s) {
			if p, pid := liveHolder(t, s); p != "" && holdingNow(t, p, bootNow(t)) {
				return p, pid
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no holder early in its turn")
	return "", 0
}

func ms(d uint64) string { return fmt.Sprintf("%.0f ms", float64(d)/1e6) }

const exclArgs = "--exclusive"

// S14: exclusive resources under round-robin, holder failures and agent
// failures. The invariant, judged from the clients' own logs: holding
// intervals never overlap.
func TestS14Exclusive(t *testing.T) {
	reset(t)
	agentArgs := []string{"--rate=10", "--rescan=2s", "--lease=3s", "--exclusive", "--hold=4s", "--deadline=8s"}
	deployAgent(t, agentArgs...)
	deployClients(t, 3, exclArgs, "--acquire-ms=300")
	cl := pods(t, clientNS, "app="+clientDP)
	s := waitSummary(t, 30*time.Second, "attached", allAttached(t, cl))
	node := s.Node
	killed := map[string]uint64{}

	// Round-robin.
	time.Sleep(15 * time.Second)
	for _, p := range cl {
		n := 0
		for _, e := range allClientEvents(t, p) {
			if e.Ev == "acquire" {
				n++
			}
		}
		if n == 0 {
			t.Errorf("round-robin: %s never acquired", p)
		}
	}

	// Holder SIGKILLed mid-grant: re-granted right after its pidfd exit.
	hp, hpid := waitHolder(t, cl)
	killed[hp] = bootNow(t) // before the signal: an acquire earlier than this is a real overlap
	signal(t, node, "KILL", hpid)
	who, at := waitAcquireAfter(t, cl, hp, killed[hp], 10*time.Second, "after the holder was killed")
	t.Logf("holder %s killed: %s acquired %s later", hp, who, ms(at-killed[hp]))
	if at-killed[hp] > uint64(1500*time.Millisecond) {
		t.Errorf("re-grant after a killed holder took %s", ms(at-killed[hp]))
	}
	kubectl(t, "-n", clientNS, "rollout", "status", "deployment/"+clientDP, "--timeout=60s")

	// Holder SIGSTOPped: no re-grant while it lives; blocked time reported.
	hp, hpid = waitHolder(t, cl)
	signal(t, node, "STOP", hpid)
	stopped := bootNow(t)
	time.Sleep(11 * time.Second) // > deadline
	if who, at, ok := acquiresAfter(t, cl, hp, stopped); ok {
		t.Errorf("%s acquired %s after holder %s was stopped", who, ms(at-stopped), hp)
	}
	s, _ = latestSummary(t)
	if len(s.Resources) == 0 || s.Resources[0].BlockedMS < 3000 {
		t.Errorf("stopped holder: blocked time not reported: %+v", s.Resources)
	} else {
		t.Logf("holder %s stopped: no re-grant for 10 s, blocked %.0f ms reported", hp, s.Resources[0].BlockedMS)
	}
	signal(t, node, "CONT", hpid)
	resumed := bootNow(t)
	who, at = waitAcquireAfter(t, cl, hp, resumed, 15*time.Second, "after the stopped holder resumed")
	t.Logf("holder resumed (deadline passed, self-released): %s acquired %s later", who, ms(at-resumed))

	// Agent SIGKILLed (mid-grant or mid-hand-off), then a rolling update.
	for i := 0; i < 3; i++ {
		prev, _ := latestSummary(t)
		signal(t, node, "KILL", prev.AgentPID)
		waitSummary(t, 60*time.Second, "agent back", func(s summary) bool {
			return s.AgentPID != prev.AgentPID && allAttached(t, cl)(s)
		})
		time.Sleep(time.Duration(1300+700*i) * time.Millisecond)
	}
	before := bootNow(t)
	kubectl(t, "-n", agentNS, "rollout", "restart", "daemonset/"+agentDS)
	kubectl(t, "-n", agentNS, "rollout", "status", "daemonset/"+agentDS, "--timeout=120s")
	who, at = waitAcquireAfter(t, cl, "", before, 30*time.Second, "after agent kills and a rolling update")
	t.Logf("after 3 agent kills and a rolling update: rotation continues (%s acquired)", who)
	time.Sleep(10 * time.Second)

	checkNoOverlap(t, holdIntervals(t, cl, killed))
}

// S14: a holder whose control thread hangs keeps the resource for as long as
// it lives (the arbiter never guesses); deleting it frees the resource.
func TestS14HungHolder(t *testing.T) {
	reset(t)
	deployAgent(t, "--rate=10", "--rescan=2s", "--lease=3s", "--exclusive", "--hold=2s", "--deadline=6s")
	hung, err := deployVariant(t, "hang", 1, []string{exclArgs, "--holder-hang"})
	if err != nil {
		t.Fatal(err)
	}
	deployClients(t, 2, exclArgs)
	cl := pods(t, clientNS, "app="+clientDP)
	deadline := time.Now().Add(60 * time.Second)
	var hangAt uint64
	for hangAt == 0 {
		for _, e := range clientEvents(t, hung[0]) {
			if e.Ev == "hang" {
				hangAt = e.TS
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the hanging holder never got the resource")
		}
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(12 * time.Second)
	if who, at, ok := acquiresAfter(t, cl, "", hangAt); ok {
		t.Errorf("%s acquired %s after the holder hung", who, ms(at-hangAt))
	}
	s, _ := latestSummary(t)
	if len(s.Resources) == 0 || s.Resources[0].BlockedMS < 5000 {
		t.Errorf("hung holder: blocked time not reported: %+v", s.Resources)
	} else {
		t.Logf("hung holder: no re-grant, blocked %.0f ms reported", s.Resources[0].BlockedMS)
	}
	// SIGKILL it on the node, for a precise end of its interval, then remove it.
	killed := map[string]uint64{hung[0]: bootNow(t)} // before the signal (see above)
	signalClient(t, s, hung[0], "KILL")
	kubectl(t, "-n", clientNS, "scale", "deployment/hang", "--replicas=0")
	who, at := waitAcquireAfter(t, cl, "", killed[hung[0]], 20*time.Second, "after the hung holder was deleted")
	t.Logf("hung holder killed: %s acquired %s later", who, ms(at-killed[hung[0]]))
	ivs := holdIntervals(t, cl, nil)
	ivs = append(ivs, interval{hung[0], hangAt, killed[hung[0]]})
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].start < ivs[j].start })
	checkNoOverlap(t, ivs)
}

// S14: a lone holder is renewed, never released.
func TestS14LoneHolder(t *testing.T) {
	d := 30 * time.Second
	if v, err := time.ParseDuration(getenv("E2E_LONE_HOLDER", "30s")); err == nil {
		d = v
	}
	reset(t)
	deployAgent(t, "--rate=10", "--rescan=2s", "--lease=3s", "--exclusive", "--hold=2s", "--deadline=6s")
	deployClients(t, 1, exclArgs)
	cl := pods(t, clientNS, "app="+clientDP)
	waitSummary(t, 30*time.Second, "holding", func(s summary) bool { p, _ := holder(s); return p != "" })
	time.Sleep(d)
	acq, rel := 0, 0
	for _, e := range clientEvents(t, cl[0]) {
		switch e.Ev {
		case "acquire":
			acq++
		case "release":
			rel++
		}
	}
	s, _ := latestSummary(t)
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
	reset(t)
	deployAgent(t, "--rate=10", "--rescan=2s", "--lease=3s", "--wake-probe=100ms")
	deployClients(t, 4, exclArgs)
	cl := pods(t, clientNS, "app="+clientDP)
	s := waitSummary(t, 60*time.Second, "100 wake samples per client", func(s summary) bool {
		if !allAttached(t, cl)(s) {
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
