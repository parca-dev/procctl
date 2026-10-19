package e2e

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// S2: clients that started before the agent are all attached by its first scan.
func TestS2ClientsBeforeAgent(t *testing.T) {
	reset(t)
	deployClients(t, 4)
	want := pods(t, clientNS, "app="+clientDP)
	start := time.Now()
	deployAgent(t)
	s := waitSummary(t, 30*time.Second, "all pre-existing clients attached", allAttached(t, want))
	t.Logf("all %d clients attached and acking %v after the agent rolled out", len(want), time.Since(start).Round(time.Second))
	checkHealthy(t, s)
	logLatencies(t, s)
}

// S3: agent restart and rolling updates. Generations stay monotonic, nothing
// tears, acks resume quickly, and with the default (RELEASED) hand-over no
// client ever falls back to its defaults.
func TestS3AgentRestartAndRollingUpdate(t *testing.T) {
	reset(t)
	deployClients(t, 4)
	want := pods(t, clientNS, "app="+clientDP)
	deployAgent(t)
	waitSummary(t, 30*time.Second, "attached", allAttached(t, want))

	steps := []struct {
		name string
		do   func()
	}{
		{"delete agent pod", func() {
			kubectl(t, "-n", agentNS, "delete", "pod", "-l", "app="+agentDS, "--wait=false")
		}},
		{"rolling update, maxSurge 0", func() {
			kubectl(t, "-n", agentNS, "rollout", "restart", "daemonset/"+agentDS)
		}},
		{"rolling update, maxSurge 1 (two agents overlap)", func() {
			kubectl(t, "-n", agentNS, "patch", "daemonset/"+agentDS, "--type=merge", "-p",
				`{"spec":{"updateStrategy":{"rollingUpdate":{"maxUnavailable":0,"maxSurge":1}}}}`)
			kubectl(t, "-n", agentNS, "rollout", "restart", "daemonset/"+agentDS)
		}},
	}
	for _, step := range steps {
		prev, _ := latestSummary(t)
		step.do()
		kubectl(t, "-n", agentNS, "rollout", "status", "daemonset/"+agentDS, "--timeout=120s")
		s := waitSummary(t, 30*time.Second, step.name+": new agent attached and acking", func(s summary) bool {
			return s.AgentPID != prev.AgentPID && allAttached(t, want)(s)
		})
		checkHealthy(t, s)
		t.Logf("%s: new agent (pid %d) owns all %d clients", step.name, s.AgentPID, s.Owned)
	}
	for _, p := range want {
		if n := len(leaseEvents(t, p, true)); n > 1 { // 1 = the initial "no agent yet"
			t.Errorf("%s fell back to defaults during a hand-over (%d expiries)", p, n-1)
		}
	}
}

// S4: a restarted client is detached (pidfd) and its replacement attached; the
// agent doesn't leak mappings.
func TestS4ClientRestart(t *testing.T) {
	reset(t)
	deployClients(t, 3)
	want := pods(t, clientNS, "app="+clientDP)
	deployAgent(t)
	waitSummary(t, 30*time.Second, "attached", allAttached(t, want))
	before, _ := latestSummary(t)
	kubectl(t, "-n", clientNS, "delete", "pod", want[0])
	kubectl(t, "-n", clientNS, "rollout", "status", "deployment/"+clientDP, "--timeout=120s")
	now := pods(t, clientNS, "app="+clientDP)
	s := waitSummary(t, 30*time.Second, "replacement attached, old one detached", func(s summary) bool {
		_, old := byPod(s)[want[0]]
		return !old && s.Detached > before.Detached && allAttached(t, now)(s)
	})
	checkHealthy(t, s)
}

// S5: the lease. With the agent gone (graceful stop), clients fall back to
// their defaults after the lease; with the agent hung (SIGSTOP), too; when it
// resumes or returns, they recover and re-apply the current configuration.
func TestS5Lease(t *testing.T) {
	reset(t)
	deployClients(t, 2)
	want := pods(t, clientNS, "app="+clientDP)
	deployAgent(t, "--rate=10", "--rescan=2s", "--lease=3s")
	s := waitSummary(t, 30*time.Second, "attached", allAttached(t, want))
	node := s.Node

	expiries := func() int {
		n := 0
		for _, p := range want {
			n += len(leaseEvents(t, p, true))
		}
		return n
	}

	// Hung agent: SIGSTOP the agent process on the node.
	base := expiries()
	pid := strings.TrimSpace(nodeExec(t, node, "pgrep", "-f", "/procctl-testagent"))
	nodeExec(t, node, "kill", "-STOP", pid)
	stopped := time.Now()
	deadline := time.Now().Add(15 * time.Second)
	for expiries() < base+len(want) {
		if time.Now().After(deadline) {
			nodeExec(t, node, "kill", "-CONT", pid)
			t.Fatal("clients did not fall back to defaults while the agent was stopped")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("SIGSTOP: all clients fell back to defaults %v after the agent stopped (lease 3s)", time.Since(stopped).Round(100*time.Millisecond))
	nodeExec(t, node, "kill", "-CONT", pid)
	s = waitSummary(t, 20*time.Second, "clients recovered and re-applied the current config", func(s summary) bool {
		for _, c := range s.Clients {
			if c.LeaseExpired || c.Redelivered == 0 {
				return false
			}
		}
		return allAttached(t, want)(s)
	})
	checkHealthy(t, s)

	// Agent gone: move the DaemonSet off the node.
	base = expiries()
	kubectl(t, "-n", agentNS, "patch", "daemonset/"+agentDS, "--type=merge", "-p",
		`{"spec":{"template":{"spec":{"nodeSelector":{"procctl-e2e/none":"x"}}}}}`)
	gone := time.Now()
	deadline = time.Now().Add(30 * time.Second)
	for expiries() < base+len(want) {
		if time.Now().After(deadline) {
			t.Fatal("clients did not fall back to defaults after the agent left")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("agent removed: clients fell back to defaults %v later", time.Since(gone).Round(100*time.Millisecond))
	kubectl(t, "-n", agentNS, "patch", "daemonset/"+agentDS, "--type=json", "-p",
		`[{"op":"remove","path":"/spec/template/spec/nodeSelector"}]`)
	kubectl(t, "-n", agentNS, "rollout", "status", "daemonset/"+agentDS, "--timeout=120s")
	s = waitSummary(t, 30*time.Second, "clients recovered with the new agent", allAttached(t, want))
	checkHealthy(t, s)
}

// S6: many clients on one node.
func TestS6Scale(t *testing.T) {
	n := 64
	if v, err := strconv.Atoi(getenv("E2E_SCALE_CLIENTS", "64")); err == nil {
		n = v
	}
	reset(t)
	deployAgent(t)
	start := time.Now()
	deployClients(t, n)
	want := pods(t, clientNS, "app="+clientDP)
	if len(want) != n {
		t.Fatalf("%d of %d client pods running", len(want), n)
	}
	s := waitSummary(t, 90*time.Second, "all clients attached", allAttached(t, want))
	t.Logf("%d clients attached %v after the deployment started", n, time.Since(start).Round(time.Second))
	checkHealthy(t, s)
	logLatencies(t, s)
}
