package e2e

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// S9: discovery cost on a busy node: many processes with many fds (the /proc
// scan), and an exec storm (mapping events) while new clients start.
// E2E_NOISE_PROCS processes × 200 fds (default 1000; 5000 long).
//
// Not parallel: it loads the whole node, so it runs alone, before the
// parallel scenarios.
func TestS9ScanCost(t *testing.T) {
	procs := 1000
	if long {
		procs = 5000
	}
	if v, err := strconv.Atoi(getenv("E2E_NOISE_PROCS", "")); err == nil {
		procs = v
	}
	const perPod, fds = 100, 200
	e := newScenario(t, "s9")
	t.Cleanup(func() { // the load must not outlive the test, whatever happens
		kubectlErr("-n", e.clientNS, "delete", "deployment/noise", "deployment/storm", "--wait=false")
	})
	e.agent(agentDS, nil, "--rescan=5s")
	want := e.clients(clientDP, 4)
	base := e.wait(30*time.Second, "attached", allAttached(want))

	noisePods := (procs + perPod - 1) / perPod
	e.clients("noise", noisePods, fmt.Sprintf("--noise-procs=%d", perPod), fmt.Sprintf("--noise-fds=%d", fds))
	s := e.wait(90*time.Second, "a full scan with the noise", func(s summary) bool {
		return s.Scan.Count > base.Scan.Count && s.Scan.Procs >= base.Scan.Procs+procs && s.Scan.FDs >= procs*fds
	})
	t.Logf("full /proc scan: %d processes, %d fds in %.0f ms (agent rss %d KiB)", s.Scan.Procs, s.Scan.FDs, s.Scan.MS, s.RSSKB)
	if limit := float64(procs); s.Scan.MS > limit { // ~1 ms per process with 200 fds
		t.Errorf("full scan took %.0f ms (limit %.0f)", s.Scan.MS, limit)
	}
	checkHealthy(t, s)

	// Exec storm, and new clients starting during it.
	before := s
	start := time.Now()
	storm := e.clients("storm", 1, "--exec-rate=1000")
	time.Sleep(3 * time.Second)
	fresh := e.clients("fresh", 2)
	s = e.wait(30*time.Second, "new clients attached during the storm", allAttached(append(want, fresh...)))
	time.Sleep(5 * time.Second)
	s, _ = e.latest()
	elapsed := time.Since(start).Seconds()
	var execs uint64
	for _, ev := range e.events(storm[0]) {
		if ev.Ev == "execs" {
			execs = ev.Total
		}
	}
	cpu := (s.CPUS - before.CPUS) / elapsed
	var events uint64
	if s.Hello != nil && before.Hello != nil {
		events = s.Hello.MappingEvents - before.Hello.MappingEvents
	}
	t.Logf("exec storm: ~%.0f exec/s, %d mapping events; agent cpu %.1f%% of a core (incl. the 5 s rescan), rss %d KiB",
		float64(execs)/elapsed, events, 100*cpu, s.RSSKB)
	if cpu > 1 {
		t.Errorf("agent used %.0f%% of a core during the exec storm", 100*cpu)
	}
	e.checkDiscoveryLatency("S9 new clients during the storm", fresh, len(fresh), 1000)
	checkHealthy(t, s)
}
