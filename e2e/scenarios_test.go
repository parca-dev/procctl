package e2e

import (
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// S2: clients that started before the agent are all attached by its first scan.
func TestS2ClientsBeforeAgent(t *testing.T) {
	t.Parallel()
	e := newScenario(t, "s2")
	want := e.clients(clientDP, 4)
	start := time.Now()
	e.agent(agentDS, nil)
	s := e.wait(30*time.Second, "all pre-existing clients attached", allAttached(want))
	t.Logf("all %d clients attached and acking %v after the agent rolled out", len(want), time.Since(start).Round(100*time.Millisecond))
	checkHealthy(t, s)
	logLatencies(t, s)
}

// S3: agent restart and rolling updates. Generations stay monotonic, nothing
// tears, and acks resume. A rolling update with overlap (maxSurge 1) hands
// over (RELEASED) without any client falling back to its defaults; a pod
// delete leaves a gap until the new pod runs, so clients fall back only if
// that gap exceeds the lease.
func TestS3AgentRestartAndRollingUpdate(t *testing.T) {
	t.Parallel()
	e := newScenario(t, "s3")
	want := e.clients(clientDP, 4)
	// The production lease: a pod delete has an inherent gap until the new
	// pod runs, which on a loaded node can exceed a short test lease.
	e.agent(agentDS, nil, "--lease=10s")
	e.wait(30*time.Second, "attached", allAttached(want))

	steps := []struct {
		name    string
		overlap bool // agents overlap: no client may fall back
		do      func()
	}{
		{"delete agent pod", false, func() {
			kubectl(t, "-n", e.agentNS, "delete", "pod", "-l", "app="+agentDS, "--wait=false")
		}},
		{"rolling update, maxSurge 1 (two agents overlap)", true, func() {
			kubectl(t, "-n", e.agentNS, "patch", "daemonset/"+agentDS, "--type=merge", "-p",
				`{"spec":{"updateStrategy":{"rollingUpdate":{"maxUnavailable":0,"maxSurge":1}}}}`)
			kubectl(t, "-n", e.agentNS, "rollout", "restart", "daemonset/"+agentDS)
		}},
	}
	if long {
		steps = append(steps, struct {
			name    string
			overlap bool
			do      func()
		}{"rolling update, maxSurge 0", false, func() {
			kubectl(t, "-n", e.agentNS, "patch", "daemonset/"+agentDS, "--type=merge", "-p",
				`{"spec":{"updateStrategy":{"rollingUpdate":{"maxUnavailable":1,"maxSurge":0}}}}`)
			kubectl(t, "-n", e.agentNS, "rollout", "restart", "daemonset/"+agentDS)
		}})
	}
	expiries := func() int {
		n := 0
		for _, p := range want {
			n += len(e.leaseEvents(p, true))
		}
		return n
	}
	for _, step := range steps {
		prev, _ := e.latest()
		before := expiries()
		started := time.Now()
		step.do()
		kubectl(t, "-n", e.agentNS, "rollout", "status", "daemonset/"+agentDS, "--timeout=180s")
		s := e.wait(120*time.Second, step.name+": new agent attached and acking", func(s summary) bool {
			return s.AgentPID != prev.AgentPID && allAttached(want)(s)
		})
		checkHealthy(t, s)
		fell := expiries() - before
		t.Logf("%s: new agent (pid %d) owns all %d clients %v later; %d fell back meanwhile",
			step.name, s.AgentPID, s.Owned, time.Since(started).Round(100*time.Millisecond), fell)
		if step.overlap && fell > 0 {
			t.Errorf("%s: %d clients fell back to defaults during an overlapping hand-over", step.name, fell)
		}
	}
}

// S4: a restarted client is detached (pidfd) and its replacement attached; the
// agent doesn't leak mappings.
func TestS4ClientRestart(t *testing.T) {
	t.Parallel()
	e := newScenario(t, "s4")
	want := e.clients(clientDP, 3)
	e.agent(agentDS, nil)
	e.wait(30*time.Second, "attached", allAttached(want))
	before, _ := e.latest()
	kubectl(t, "-n", e.clientNS, "delete", "pod", want[0])
	kubectl(t, "-n", e.clientNS, "rollout", "status", "deployment/"+clientDP, "--timeout=120s")
	now := e.pods(e.clientNS, "app="+clientDP)
	s := e.wait(30*time.Second, "replacement attached, old one detached", func(s summary) bool {
		_, old := byPod(s)[want[0]]
		return !old && s.Detached > before.Detached && allAttached(now)(s)
	})
	checkHealthy(t, s)
}

// S5: the lease. With the agent hung (SIGSTOP), clients fall back to their
// defaults after the lease; when it resumes they recover and re-apply the
// current configuration. With the agent gone, too.
func TestS5Lease(t *testing.T) {
	t.Parallel()
	const lease = time.Second
	e := newScenario(t, "s5")
	want := e.clients(clientDP, 2)
	e.agent(agentDS, nil, "--lease="+lease.String())
	s := e.wait(30*time.Second, "attached", allAttached(want))
	node := s.Node

	expiries := func() int {
		n := 0
		for _, p := range want {
			n += len(e.leaseEvents(p, true))
		}
		return n
	}
	waitExpiries := func(n int, what string, onFail func()) time.Duration {
		start := time.Now()
		for expiries() < n {
			if time.Since(start) > 45*time.Second { // includes the pod's termination
				if onFail != nil {
					onFail()
				}
				t.Fatalf("clients did not fall back to defaults: %s", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
		return time.Since(start)
	}

	// Hung agent.
	base := expiries()
	stoppedAt, _ := e.latest()
	signal(t, node, "STOP", s.AgentPID)
	d := waitExpiries(base+len(want), "agent stopped", func() { signal(t, node, "CONT", s.AgentPID) })
	t.Logf("SIGSTOP: all clients fell back to defaults %v after the agent stopped (lease %v)", d.Round(100*time.Millisecond), lease)
	signal(t, node, "CONT", s.AgentPID)
	// Recovery re-applies the current configuration: either redelivered (the
	// same generation, SPEC §8.3) or a newer one the resumed agent wrote first.
	before := byPod(stoppedAt)
	s = e.wait(20*time.Second, "clients recovered and re-applied the configuration", func(s summary) bool {
		for _, c := range s.Clients {
			if c.LeaseExpired || (c.Redelivered == 0 && c.Changes <= before[c.Pod].Changes) {
				return false
			}
		}
		return allAttached(want)(s)
	})
	checkHealthy(t, s)

	// Agent gone: move the DaemonSet off the node.
	base = expiries()
	kubectl(t, "-n", e.agentNS, "patch", "daemonset/"+agentDS, "--type=merge", "-p",
		`{"spec":{"template":{"spec":{"nodeSelector":{"procctl-e2e/none":"x"}}}}}`)
	d = waitExpiries(base+len(want), "agent removed", nil)
	t.Logf("agent removed: clients fell back to defaults %v later", d.Round(100*time.Millisecond))
	kubectl(t, "-n", e.agentNS, "patch", "daemonset/"+agentDS, "--type=json", "-p",
		`[{"op":"remove","path":"/spec/template/spec/nodeSelector"}]`)
	kubectl(t, "-n", e.agentNS, "rollout", "status", "daemonset/"+agentDS, "--timeout=120s")
	s = e.wait(60*time.Second, "clients recovered with the new agent", allAttached(want))
	checkHealthy(t, s)
}

// S6: many clients on one node (E2E_SCALE_CLIENTS; default 32, 200 long).
func TestS6Scale(t *testing.T) {
	t.Parallel()
	n := 32
	if long {
		n = 200
	}
	if v, err := strconv.Atoi(getenv("E2E_SCALE_CLIENTS", "")); err == nil {
		n = v
	}
	e := newScenario(t, "s6")
	e.agent(agentDS, nil)
	start := time.Now()
	want, err := e.clientsWithin(180*time.Second, clientDP, n, func(d *appsv1.Deployment) {
		// Keep 200 clients' requests within a small node.
		d.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != n {
		t.Fatalf("%d of %d client pods running", len(want), n)
	}
	s := e.wait(90*time.Second, "all clients attached", allAttached(want))
	t.Logf("%d clients attached %v after the deployment started", n, time.Since(start).Round(time.Second))
	checkHealthy(t, s)
	logLatencies(t, s)
}
