package e2e

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// S10: the client is an ordinary restricted workload (PSA restricted and
// RuntimeDefault seccomp are the base manifests); here also in its own user
// namespace (hostUsers: false), where the cluster supports it.
func TestS10UserNamespace(t *testing.T) {
	t.Parallel()
	e := newScenario(t, "s10")
	e.agent(agentDS, nil)
	no := false
	ps, err := e.clientsWithin(40*time.Second, clientDP, 2, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.HostUsers = &no
	})
	if err != nil {
		// kind: runc can't mount sysfs for a user-namespaced pod inside the
		// node container. Runs on real nodes (M4).
		ev, _ := kubectlErr("-n", e.clientNS, "get", "events", "--field-selector=reason=FailedCreatePodSandBox",
			"-o", "jsonpath={.items[-1:].message}")
		t.Skipf("hostUsers: false pods don't start on this cluster: %s", lastLines(ev, 1))
	}
	s := e.wait(30*time.Second, "user-namespaced clients attached", allAttached(ps))
	checkHealthy(t, s)
	for _, p := range ps {
		c := byPod(s)[p]
		t.Logf("%s: host pid %d, ns pids %v, attach %s, read %s", p, c.HostPID, c.NSPid, c.Path, c.ReadPath)
	}
	logLatencies(t, s)
}

// S11: every attach path × read path.
func TestS11AttachAndReadPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id           string
		args         []string
		attach, read string
	}{
		{"s11-pidfd-vm", nil, "pidfd_getfd", "process_vm_readv"},
		{"s11-proc-vm", []string{"--force-proc-fallback"}, "proc", "process_vm_readv"},
		{"s11-pidfd-mem", []string{"--force-mem-fallback"}, "pidfd_getfd", "proc_mem"},
		{"s11-proc-mem", []string{"--force-proc-fallback", "--force-mem-fallback"}, "proc", "proc_mem"},
	} {
		t.Run(tc.attach+"+"+tc.read, func(t *testing.T) {
			t.Parallel()
			e := newScenario(t, tc.id)
			want := e.clients(clientDP, 2)
			e.agent(agentDS, nil, tc.args...)
			s := e.wait(30*time.Second, tc.attach+" + "+tc.read, func(s summary) bool {
				if !allAttached(want)(s) {
					return false
				}
				for _, c := range s.Clients {
					if c.Path != tc.attach || c.ReadPath != tc.read || c.Samples < 5 {
						return false
					}
				}
				return true
			})
			checkHealthy(t, s)
		})
	}
}

// S12: missing privileges degrade, never crash. Without CAP_SYS_ADMIN hello
// uprobes are unavailable (mapping events + rescans); without BPF/PERFMON
// discovery is scan-only; without SYS_PTRACE nothing can attach, and clients
// simply run on their defaults.
func TestS12Degraded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id        string
		caps      []corev1.Capability
		discovery string
		attaches  bool
	}{
		{"s12-no-admin", []corev1.Capability{"SYS_PTRACE", "BPF", "PERFMON"}, "mmap-only", true},
		{"s12-no-bpf", []corev1.Capability{"SYS_PTRACE"}, "scan-only", true},
		{"s12-no-ptrace", []corev1.Capability{"BPF", "PERFMON", "SYS_ADMIN"}, "", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			e := newScenario(t, tc.id)
			want := e.clients(clientDP, 2)
			e.agent(agentDS, func(ds *appsv1.DaemonSet) {
				ds.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add = tc.caps
			})
			if tc.attaches {
				s := e.wait(30*time.Second, "attached", allAttached(want))
				checkHealthy(t, s)
				if s.Discovery != tc.discovery {
					t.Errorf("discovery %q, want %q", s.Discovery, tc.discovery)
				}
				t.Logf("caps %v: discovery %s, %d clients attached", tc.caps, s.Discovery, s.Attached)
				return
			}
			time.Sleep(6 * time.Second) // three rescans
			s, ok := e.latest()
			if !ok {
				t.Fatal("agent prints no summary")
			}
			if s.Attached != 0 {
				t.Errorf("%d attached without SYS_PTRACE", s.Attached)
			}
			if n := e.restarts(e.agentNS, "app="+agentDS); n != 0 {
				t.Errorf("agent restarted %d times", n)
			}
			for _, p := range want {
				if ev := e.events(p); len(ev) == 0 || ev[len(ev)-1].Ev == "exit" {
					t.Errorf("%s not running", p)
				}
			}
			t.Logf("caps %v: agent up (discovery %s), nothing attached, clients on their defaults", tc.caps, s.Discovery)
		})
	}
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}
