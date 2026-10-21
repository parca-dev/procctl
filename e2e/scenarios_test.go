package e2e

import (
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Push and ack: clients started before and after the agent are found, take
// every payload intact and in order, and ack it; the agent maps each client
// to the process it really is; a restarted client is detached and its
// replacement attached.
func TestPushAndAck(t *testing.T) {
	t.Parallel()
	e := newScenario(t, "push")
	before := e.clients(clientDP, 4)
	e.agent(agentDS, nil)
	after := e.clients("late", 4)
	all := append(append([]string{}, before...), after...)
	s := e.wait(30*time.Second, "all clients attached and acking", allAttached(all))
	checkHealthy(t, s)
	logLatencies(t, s)

	kubectl(t, "-n", e.clientNS, "delete", "pod", before[0])
	kubectl(t, "-n", e.clientNS, "rollout", "status", "deployment/"+clientDP, "--timeout=120s")
	now := append(e.pods(e.clientNS, "app="+clientDP), after...)
	s = e.wait(30*time.Second, "replacement attached, old one detached", func(s summary) bool {
		_, old := byPod(s)[before[0]]
		return !old && s.Detached > 0 && allAttached(now)(s)
	})
	checkHealthy(t, s)
}

// Agent restart: an overlapping rolling update hands over (RELEASED) without
// any client falling back to its defaults; after a plain pod delete the new
// agent takes over and pushes again.
func TestAgentRestart(t *testing.T) {
	t.Parallel()
	e := newScenario(t, "restart")
	want := e.clients(clientDP, 4)
	e.agent(agentDS, func(ds *appsv1.DaemonSet) {
		one, zero := intstr.FromInt32(1), intstr.FromInt32(0)
		ds.Spec.UpdateStrategy.RollingUpdate.MaxSurge = &one
		ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable = &zero
	}, "--lease=10s")
	e.wait(30*time.Second, "attached", allAttached(want))
	expiries := func() int {
		n := 0
		for _, p := range want {
			n += len(e.leaseEvents(p, true))
		}
		return n
	}

	prev, _ := e.latest()
	fellBefore := expiries()
	kubectl(t, "-n", e.agentNS, "rollout", "restart", "daemonset/"+agentDS)
	kubectl(t, "-n", e.agentNS, "rollout", "status", "daemonset/"+agentDS, "--timeout=180s")
	s := e.wait(60*time.Second, "new agent attached and acking", func(s summary) bool {
		return s.AgentPID != prev.AgentPID && allAttached(want)(s)
	})
	checkHealthy(t, s)
	if n := expiries() - fellBefore; n > 0 {
		t.Errorf("%d clients fell back to defaults during an overlapping hand-over", n)
	}

	prev = s
	kubectl(t, "-n", e.agentNS, "delete", "pod", "-l", "app="+agentDS, "--wait=false")
	s = e.wait(120*time.Second, "agent back after a pod delete", func(s summary) bool {
		return s.AgentPID != prev.AgentPID && allAttached(want)(s)
	})
	checkHealthy(t, s)
}

// Lease: when the agent dies without releasing (--release=false: as after a
// crash), clients fall back to their defaults within the lease; when an agent
// returns they recover and apply its configuration.
func TestLeaseFallback(t *testing.T) {
	t.Parallel()
	const lease = 2 * time.Second
	e := newScenario(t, "lease")
	want := e.clients(clientDP, 2)
	e.agent(agentDS, nil, "--lease="+lease.String(), "--release=false")
	prev := e.wait(30*time.Second, "attached", allAttached(want))

	expired := func() bool {
		for _, p := range want {
			ev := e.leaseEvents(p, true)
			if len(ev) < 2 { // the first is "no agent yet"
				return false
			}
		}
		return true
	}
	kubectl(t, "-n", e.agentNS, "delete", "pod", "-l", "app="+agentDS, "--wait=false")
	gone := time.Now()
	for !expired() {
		if time.Since(gone) > 60*time.Second {
			t.Fatal("clients did not fall back to their defaults")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("agent gone: all clients on their defaults %v later (lease %v)", time.Since(gone).Round(100*time.Millisecond), lease)
	s := e.wait(120*time.Second, "clients recovered with the new agent", func(s summary) bool {
		if s.AgentPID == prev.AgentPID || !allAttached(want)(s) {
			return false
		}
		for _, c := range s.Clients {
			if c.LeaseExpired {
				return false
			}
		}
		return true
	})
	checkHealthy(t, s)
}

// Two agents: exactly one owns each region and ownership doesn't move while
// it is healthy; when the owner dies without releasing, the other takes over
// after the lease, and the restarted one stays out.
func TestTwoAgents(t *testing.T) {
	t.Parallel()
	const agentB = "procctl-testagent-b"
	args := []string{"--lease=2s", "--release=false"}
	e := newScenario(t, "two")
	want := e.clients(clientDP, 4)
	e.agent(agentDS, nil, args...)
	a := e.wait(30*time.Second, "agent A owns all", ownsAll(want))
	e.agent(agentB, nil, args...)
	e.waitOf(agentB, 30*time.Second, "agent B sees all, owns none", func(s summary) bool {
		return s.Attached == len(want) && s.Owned == 0
	})
	time.Sleep(3 * time.Second)
	a, _ = e.latest()
	b, _ := e.latestOf(agentB)
	if a.Owned != len(want) || b.Owned != 0 || changes(a) != 0 {
		t.Fatalf("while healthy: A owns %d (%d changes), B owns %d", a.Owned, changes(a), b.Owned)
	}

	kubectl(t, "-n", e.agentNS, "delete", "pod", "-l", "app="+agentDS, "--wait=false")
	killed := time.Now()
	e.waitOf(agentB, 60*time.Second, "B took over", ownsAll(want))
	t.Logf("A gone: B owns all %v later (lease 2s)", time.Since(killed).Round(100*time.Millisecond))
	e.wait(120*time.Second, "A restarted", func(s summary) bool { return s.AgentPID != a.AgentPID && s.Attached == len(want) })
	time.Sleep(3 * time.Second)
	a, _ = e.latest()
	b, _ = e.latestOf(agentB)
	if a.Owned != 0 || b.Owned != len(want) {
		t.Errorf("after A restarted: A owns %d, B owns %d", a.Owned, b.Owned)
	}
	checkHealthy(t, b)
}

// Fork: children that keep the inherited region are never attached as
// clients of their own and can't write its state block (-ECHILD); children
// that re-initialize get their own region and are attached.
func TestFork(t *testing.T) {
	t.Parallel()
	const replicas, workers = 2, 4
	t.Run("inherit", func(t *testing.T) {
		t.Parallel()
		e := newScenario(t, "fork-inherit")
		e.agent(agentDS, nil)
		want := e.clients(clientDP, replicas, "--fork-after-ms=1000", fmt.Sprintf("--fork-workers=%d", workers))
		e.wait(30*time.Second, "parents attached", allAttached(want))
		time.Sleep(3 * time.Second)
		s, _ := e.latest()
		checkHealthy(t, s)
		if len(s.Clients) != replicas {
			t.Errorf("%d clients attached, want only the %d parents", len(s.Clients), replicas)
		}
		for _, p := range want {
			n := 0
			for _, ev := range e.events(p) {
				if ev.Ev == "forked_child" {
					n++
					if ev.InheritedAck != -10 {
						t.Errorf("%s child %d: inherited ack returned %d, want -ECHILD", p, ev.PID, ev.InheritedAck)
					}
				}
			}
			if n != workers {
				t.Errorf("%s: %d forked children, want %d", p, n, workers)
			}
		}
	})
	t.Run("reinit", func(t *testing.T) {
		t.Parallel()
		e := newScenario(t, "fork-reinit")
		e.agent(agentDS, nil)
		want := e.clients(clientDP, replicas, "--fork-after-ms=1000", fmt.Sprintf("--fork-workers=%d", workers), "--fork-reinit")
		all := append([]string{}, want...)
		for _, p := range want {
			for k := 0; k < workers; k++ {
				all = append(all, fmt.Sprintf("%s-child%d", p, k))
			}
		}
		s := e.wait(30*time.Second, "parents and reinit children attached", allAttached(all))
		checkHealthy(t, s)
	})
}

func ownsAll(want []string) func(summary) bool {
	return func(s summary) bool { return allAttached(want)(s) && s.Owned == len(want) }
}

func changes(s summary) int {
	n := 0
	for _, c := range s.Clients {
		n += c.OwnChanges
	}
	return n
}
