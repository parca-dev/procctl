package e2e

import (
	"testing"
	"time"
)

const agentB = "procctl-testagent-b"

// S13: two agents on one node. Exactly one owns each region, ownership never
// moves while the owner is healthy, a stopped owner is taken over after the
// lease, a resumed one doesn't take back, and a killed one is replaced.
func TestS13TwoAgents(t *testing.T) {
	t.Parallel()
	const lease = "--lease=2s"
	stable := 3 * time.Second
	if long {
		stable = 10 * time.Second
	}
	e := newScenario(t, "s13")
	want := e.clients(clientDP, 4)
	e.agent(agentDS, nil, lease)
	a := e.wait(30*time.Second, "agent A owns all", ownsAll(want))
	node := a.Node

	e.agent(agentB, nil, lease)
	e.waitOf(agentB, 30*time.Second, "agent B attached, owning nothing", func(s summary) bool {
		return s.Attached == len(want) && s.Owned == 0
	})
	time.Sleep(stable)
	a, _ = e.latest()
	b, _ := e.latestOf(agentB)
	if a.Owned != len(want) || b.Owned != 0 {
		t.Fatalf("while healthy: A owns %d, B owns %d; want %d and 0", a.Owned, b.Owned, len(want))
	}
	for _, c := range a.Clients {
		if c.OwnChanges != 0 {
			t.Errorf("%s changed owner %d times while A was healthy", c.Pod, c.OwnChanges)
		}
	}
	t.Logf("healthy: A owns all %d, B owns none, no ownership changes", a.Owned)

	// Owner hung: B takes over after the lease; A, resumed, stays out.
	signal(t, node, "STOP", a.AgentPID)
	stopped := time.Now()
	e.waitOf(agentB, 20*time.Second, "B took over from the stopped A", ownsAll(want))
	t.Logf("A stopped: B owns all %v later (lease 2s)", time.Since(stopped).Round(100*time.Millisecond))
	signal(t, node, "CONT", a.AgentPID)
	e.wait(20*time.Second, "resumed A owns nothing", func(s summary) bool { return s.Owned == 0 })
	b1, _ := e.latestOf(agentB)
	time.Sleep(stable)
	b2, _ := e.latestOf(agentB)
	a, _ = e.latest()
	if a.Owned != 0 || b2.Owned != len(want) || changes(b2) != changes(b1) {
		t.Errorf("after A resumed: A owns %d, B owns %d, B ownership changes %d → %d",
			a.Owned, b2.Owned, changes(b1), changes(b2))
	}

	// Owner killed: A takes over; the restarted B stays out.
	signal(t, node, "KILL", b2.AgentPID)
	killed := time.Now()
	e.wait(20*time.Second, "A took over from the killed B", ownsAll(want))
	t.Logf("B killed: A owns all %v later", time.Since(killed).Round(100*time.Millisecond))
	e.waitOf(agentB, 60*time.Second, "B restarted", func(s summary) bool { return s.AgentPID != b2.AgentPID })
	time.Sleep(stable)
	a, _ = e.latest()
	b, _ = e.latestOf(agentB)
	if a.Owned != len(want) || b.Owned != 0 {
		t.Errorf("after B restarted: A owns %d, B owns %d", a.Owned, b.Owned)
	}
	checkHealthy(t, a)
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
