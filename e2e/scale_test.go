package e2e

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// S9: discovery cost on a busy node: many processes with many fds (the /proc
// scan), and an exec storm (mapping events) while new clients start. Reduced
// scale on kind: E2E_NOISE_PROCS (default 1000) processes × 200 fds.
func TestS9ScanCost(t *testing.T) {
	procs, _ := strconv.Atoi(getenv("E2E_NOISE_PROCS", "1000"))
	const perPod, fds = 100, 200
	reset(t)
	deployAgent(t, "--rate=10", "--rescan=5s", "--lease=3s")
	deployClients(t, 4)
	want := pods(t, clientNS, "app="+clientDP)
	base := waitSummary(t, 30*time.Second, "attached", allAttached(t, want))

	noisePods := (procs + perPod - 1) / perPod
	if _, err := deployVariant(t, "noise", noisePods, []string{
		fmt.Sprintf("--noise-procs=%d", perPod), fmt.Sprintf("--noise-fds=%d", fds)}); err != nil {
		t.Fatal(err)
	}
	s := waitSummary(t, 60*time.Second, "a full scan with the noise", func(s summary) bool {
		return s.Scan.Count > base.Scan.Count && s.Scan.Procs >= base.Scan.Procs+procs && s.Scan.FDs >= procs*fds
	})
	t.Logf("full /proc scan: %d processes, %d fds in %.0f ms (agent rss %d KiB)", s.Scan.Procs, s.Scan.FDs, s.Scan.MS, s.RSSKB)
	if s.Scan.MS > 5000 {
		t.Errorf("full scan took %.0f ms", s.Scan.MS)
	}
	checkHealthy(t, s)

	// Exec storm, and new clients starting during it.
	before := s
	start := time.Now()
	storm, err := deployVariant(t, "storm", 1, []string{"--exec-rate=1000"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	kubectl(t, "-n", clientNS, "scale", "deployment/"+clientDP, fmt.Sprintf("--replicas=%d", len(want)+2))
	kubectl(t, "-n", clientNS, "rollout", "status", "deployment/"+clientDP, "--timeout=120s")
	now := pods(t, clientNS, "app="+clientDP)
	var fresh []string
	for _, p := range now {
		if !contains(want, p) {
			fresh = append(fresh, p)
		}
	}
	s = waitSummary(t, 30*time.Second, "new clients attached during the storm", allAttached(t, now))
	time.Sleep(10 * time.Second)
	s, _ = latestSummary(t)
	elapsed := time.Since(start).Seconds()
	var execs uint64
	for _, e := range clientEvents(t, storm[0]) {
		if e.Ev == "execs" {
			execs = e.Total
		}
	}
	cpu := (s.CPUS - before.CPUS) / elapsed
	var events uint64
	if s.Hello != nil && before.Hello != nil {
		events = s.Hello.MappingEvents - before.Hello.MappingEvents
	}
	t.Logf("exec storm: ~%.0f exec/s, %d mapping events; agent cpu %.1f%% of a core, rss %d KiB",
		float64(execs)/elapsed, events, 100*cpu, s.RSSKB)
	if cpu > 0.5 {
		t.Errorf("agent used %.0f%% of a core during the exec storm", 100*cpu)
	}
	checkDiscoveryLatency(t, "S9 new clients during the storm", fresh, len(fresh), 1000)
	checkHealthy(t, s)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
