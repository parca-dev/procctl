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
	reset(t)
	deployClients(t, 4)
	want := pods(t, clientNS, "app="+clientDP)
	deployAgent(t)
	a := waitSummary(t, 30*time.Second, "agent A owns all", ownsAll(t, want))
	node := a.Node

	deployAgentVariant(t, agentB)
	waitSummaryOf(t, agentB, 30*time.Second, "agent B attached, owning nothing", func(s summary) bool {
		return s.Attached == len(want) && s.Owned == 0
	})
	time.Sleep(10 * time.Second)
	a, _ = latestSummary(t)
	b, _ := latestSummaryOf(t, agentB)
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
	waitSummaryOf(t, agentB, 20*time.Second, "B took over from the stopped A", ownsAll(t, want))
	t.Logf("A stopped: B owns all %v later (lease 3s)", time.Since(stopped).Round(100*time.Millisecond))
	signal(t, node, "CONT", a.AgentPID)
	waitSummary(t, 20*time.Second, "resumed A owns nothing", func(s summary) bool { return s.Owned == 0 })
	b1, _ := latestSummaryOf(t, agentB)
	time.Sleep(10 * time.Second)
	b2, _ := latestSummaryOf(t, agentB)
	a, _ = latestSummary(t)
	if a.Owned != 0 || b2.Owned != len(want) || changes(b2) != changes(b1) {
		t.Errorf("after A resumed: A owns %d, B owns %d, B ownership changes %d → %d",
			a.Owned, b2.Owned, changes(b1), changes(b2))
	}

	// Owner killed: A takes over; the restarted B stays out.
	signal(t, node, "KILL", b2.AgentPID)
	killed := time.Now()
	waitSummary(t, 20*time.Second, "A took over from the killed B", ownsAll(t, want))
	t.Logf("B killed: A owns all %v later", time.Since(killed).Round(100*time.Millisecond))
	waitSummaryOf(t, agentB, 60*time.Second, "B restarted", func(s summary) bool { return s.AgentPID != b2.AgentPID })
	time.Sleep(10 * time.Second)
	a, _ = latestSummary(t)
	b, _ = latestSummaryOf(t, agentB)
	if a.Owned != len(want) || b.Owned != 0 {
		t.Errorf("after B restarted: A owns %d, B owns %d", a.Owned, b.Owned)
	}
	checkHealthy(t, a)
}

func ownsAll(t *testing.T, want []string) func(summary) bool {
	return func(s summary) bool { return allAttached(t, want)(s) && s.Owned == len(want) }
}

func changes(s summary) int {
	n := 0
	for _, c := range s.Clients {
		n += c.OwnChanges
	}
	return n
}
