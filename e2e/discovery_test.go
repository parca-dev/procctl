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
func (e *scenario) firstApplies(pod string) map[string]float64 {
	e.t.Helper()
	out := map[string]float64{}
	for _, ev := range e.events(pod) {
		if ev.Ev == "first_apply" {
			if _, seen := out[ev.Pod]; !seen {
				out[ev.Pod] = ev.SinceStartMS
			}
		}
	}
	return out
}

func (e *scenario) checkDiscoveryLatency(label string, pods []string, want int, limitMS float64) {
	e.t.Helper()
	var all []float64
	for _, p := range pods {
		for who, ms := range e.firstApplies(p) {
			all = append(all, ms)
			if ms > limitMS {
				e.t.Errorf("%s: %s first applied %.0f ms after init (limit %.0f ms)", label, who, ms, limitMS)
			}
		}
	}
	if len(all) != want {
		e.t.Errorf("%s: %d clients applied a payload, want %d", label, len(all), want)
	}
	sort.Float64s(all)
	if len(all) > 0 {
		e.t.Logf("%s: %d clients, init → first apply: median %.0f ms, max %.0f ms",
			label, len(all), all[len(all)/2], all[len(all)-1])
	}
}

func (e *scenario) requireHello() summary {
	e.t.Helper()
	s := e.wait(30*time.Second, "agent summary", func(summary) bool { return true })
	if s.Discovery != "hello" {
		e.t.Fatalf("agent runs discovery %q, want hello (BPF/perf unavailable in the agent pod?)", s.Discovery)
	}
	return s
}

// S1: cold start. With the rescan effectively off, clients that start after
// the agent are found by the mapping event (first exec of a never-seen
// binary) or the hello probe, and apply a payload < 1 s after init.
func TestS1ColdDiscovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id, label string
		args      []string
	}{
		{"s1-exec", "init at exec", nil},
		{"s1-lazy", "init 2 s after exec (lazy library init)", []string{"--init-delay-ms=2000"}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			e := newScenario(t, tc.id)
			e.agent(agentDS, nil, noRescan)
			e.requireHello()
			want := e.clients(clientDP, 4, tc.args...)
			s := e.wait(15*time.Second, tc.label+": all attached", allAttached(want))
			e.checkDiscoveryLatency("S1 "+tc.label, want, len(want), 1000)
			checkHealthy(t, s)
		})
	}
}

// S7: forked workers. Children that keep the inherited region are never
// attached as clients of their own (their state block is stale) and can't
// write it (-ECHILD); children that re-initialize get their own region and
// are found via hello in < 1 s.
func TestS7Fork(t *testing.T) {
	t.Parallel()
	const replicas, workers = 2, 8
	t.Run("inherit", func(t *testing.T) {
		t.Parallel()
		e := newScenario(t, "s7-inherit")
		e.agent(agentDS, nil, noRescan, "--exclusive", "--hold=1s", "--deadline=3s")
		e.requireHello()
		want := e.clients(clientDP, replicas, "--exclusive", "--fork-after-ms=1000", fmt.Sprintf("--fork-workers=%d", workers))
		e.wait(15*time.Second, "parents attached", allAttached(want))
		time.Sleep(3 * time.Second) // children run, exclusive turns rotate
		s, _ := e.latest()
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
			for _, ev := range e.events(p) {
				if ev.Ev == "forked_child" {
					n++
					if ev.InheritedAck != -10 {
						t.Errorf("%s child %d: ack on the inherited state block returned %d, want -ECHILD", p, ev.PID, ev.InheritedAck)
					}
				}
			}
			if n != workers {
				t.Errorf("%s: %d forked children logged, want %d", p, n, workers)
			}
		}
	})
	t.Run("reinit", func(t *testing.T) {
		t.Parallel()
		e := newScenario(t, "s7-reinit")
		e.agent(agentDS, nil, noRescan)
		e.requireHello()
		want := e.clients(clientDP, replicas, "--fork-after-ms=1000", fmt.Sprintf("--fork-workers=%d", workers), "--fork-reinit")
		all := append([]string{}, want...)
		for _, p := range want {
			for k := 0; k < workers; k++ {
				all = append(all, fmt.Sprintf("%s-child%d", p, k))
			}
		}
		s := e.wait(15*time.Second, "parents and reinit children attached", allAttached(all))
		checkHealthy(t, s)
		e.checkDiscoveryLatency("S7 reinit", want, len(all), 1000)
	})
}
