package e2e

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// noRescan effectively disables the periodic /proc scan: only the hello
// probe and mapping events can find new clients.
const noRescan = "--rescan=1h"

// firstApplies returns, per client (pod log "pod" field), the time from
// procctl_init (or fork) to the first applied payload.
func firstApplies(t *testing.T, pod string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, e := range clientEvents(t, pod) {
		if e.Ev == "first_apply" {
			if _, seen := out[e.Pod]; !seen {
				out[e.Pod] = e.SinceStartMS
			}
		}
	}
	return out
}

func checkDiscoveryLatency(t *testing.T, label string, pods []string, want int, limitMS float64) {
	t.Helper()
	var all []float64
	for _, p := range pods {
		for who, ms := range firstApplies(t, p) {
			all = append(all, ms)
			if ms > limitMS {
				t.Errorf("%s: %s first applied %.0f ms after init (limit %.0f ms)", label, who, ms, limitMS)
			}
		}
	}
	if len(all) != want {
		t.Errorf("%s: %d clients applied a payload, want %d", label, len(all), want)
	}
	sort.Float64s(all)
	if len(all) > 0 {
		t.Logf("%s: %d clients, init → first apply: median %.0f ms, max %.0f ms",
			label, len(all), all[len(all)/2], all[len(all)-1])
	}
}

func requireHello(t *testing.T) summary {
	t.Helper()
	s := waitSummary(t, 30*time.Second, "agent summary", func(summary) bool { return true })
	if s.Discovery != "hello" {
		t.Fatalf("agent runs discovery %q, want hello (BPF/perf unavailable in the agent pod?)", s.Discovery)
	}
	return s
}

// S1: cold start. With the rescan effectively off, clients that start after
// the agent are found by the mapping event (first exec of a never-seen
// binary) or the hello probe, and apply a payload < 1 s after init.
func TestS1ColdDiscovery(t *testing.T) {
	reset(t)
	deployAgent(t, noRescan)
	requireHello(t)

	for _, tc := range []struct {
		label string
		args  []string
	}{
		{"init at exec", nil},
		{"init 2 s after exec (lazy library init)", []string{"--init-delay-ms=2000"}},
	} {
		kubectlErr("delete", "namespace", clientNS, "--ignore-not-found", "--wait=true", "--timeout=120s")
		deployClients(t, 4, tc.args...)
		want := pods(t, clientNS, "app="+clientDP)
		waitSummary(t, 15*time.Second, tc.label+": all attached", allAttached(t, want))
		checkDiscoveryLatency(t, "S1 "+tc.label, want, len(want), 1000)
	}
	s, _ := latestSummary(t)
	checkHealthy(t, s)
	if s.Hello != nil {
		t.Logf("hello watcher: %+v", *s.Hello)
	}
}

// S7: forked workers. Children that keep the inherited region are never
// attached as clients of their own (their state block is stale) and can't
// write it (-ECHILD); children that re-initialize get their own region and
// are found via hello in < 1 s.
func TestS7Fork(t *testing.T) {
	const replicas, workers = 2, 8
	reset(t)
	deployAgent(t, noRescan, "--exclusive")
	requireHello(t)

	t.Run("inherit", func(t *testing.T) {
		kubectlErr("delete", "namespace", clientNS, "--ignore-not-found", "--wait=true", "--timeout=120s")
		deployClients(t, replicas, "--exclusive", "--fork-after-ms=1000", fmt.Sprintf("--fork-workers=%d", workers))
		want := pods(t, clientNS, "app="+clientDP)
		waitSummary(t, 15*time.Second, "parents attached", allAttached(t, want))
		time.Sleep(5 * time.Second) // children run, exclusive turns rotate
		s, _ := latestSummary(t)
		checkHealthy(t, s)
		if len(s.Clients) != replicas {
			t.Errorf("%d clients attached, want only the %d parents", len(s.Clients), replicas)
		}
		parents := map[int]bool{}
		for _, c := range s.Clients {
			parents[c.HostPID] = true
			if c.Stale {
				t.Errorf("parent %s read as stale", c.Pod)
			}
		}
		for _, r := range s.Resources {
			if r.HolderPID != 0 && !parents[r.HolderPID] {
				t.Errorf("resource held by pid %d, not a parent", r.HolderPID)
			}
		}
		for _, p := range want {
			n := 0
			for _, e := range clientEvents(t, p) {
				if e.Ev == "forked_child" {
					n++
					if e.InheritedAck != -10 {
						t.Errorf("%s child %d: ack on the inherited state block returned %d, want -ECHILD", p, e.PID, e.InheritedAck)
					}
				}
			}
			if n != workers {
				t.Errorf("%s: %d forked children logged, want %d", p, n, workers)
			}
		}
	})

	t.Run("reinit", func(t *testing.T) {
		kubectlErr("delete", "namespace", clientNS, "--ignore-not-found", "--wait=true", "--timeout=120s")
		deployClients(t, replicas, "--fork-after-ms=1000", fmt.Sprintf("--fork-workers=%d", workers), "--fork-reinit")
		want := pods(t, clientNS, "app="+clientDP)
		all := append([]string{}, want...)
		for _, p := range want {
			for k := 0; k < workers; k++ {
				all = append(all, fmt.Sprintf("%s-child%d", p, k))
			}
		}
		s := waitSummary(t, 15*time.Second, "parents and reinit children attached", allAttached(t, all))
		checkHealthy(t, s)
		checkDiscoveryLatency(t, "S7 reinit", want, len(all), 1000)
	})
}
