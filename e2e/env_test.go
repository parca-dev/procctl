package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// S10: the client is an ordinary restricted workload (PSA restricted and
// RuntimeDefault seccomp are the base manifests); here also in its own user
// namespace (hostUsers: false), where the cluster supports it.
func TestS10UserNamespace(t *testing.T) {
	reset(t)
	deployAgent(t)
	ps, err := deployVariantWithin(t, 40*time.Second, "userns", 2, []string{"--poll-us=1000"},
		`{"spec":{"template":{"spec":{"hostUsers":false}}}}`)
	if err != nil {
		// kind: runc can't mount sysfs for a user-namespaced pod inside the
		// node container. Runs on real nodes (M4).
		ev, _ := kubectlErr("-n", clientNS, "get", "events", "--field-selector=reason=FailedCreatePodSandBox",
			"-o", "jsonpath={.items[-1:].message}")
		kubectlErr("-n", clientNS, "delete", "deployment/userns", "--wait=false")
		t.Skipf("hostUsers: false pods don't start on this cluster: %s", lastLines(ev, 1))
	}
	s := waitSummary(t, 30*time.Second, "user-namespaced clients attached", allAttached(t, ps))
	checkHealthy(t, s)
	for _, p := range ps {
		c := byPod(s)[p]
		t.Logf("%s: host pid %d, ns pids %v, attach %s, read %s", p, c.HostPID, c.NSPid, c.Path, c.ReadPath)
	}
	logLatencies(t, s)
}

// S11: every attach path × read path.
func TestS11AttachAndReadPaths(t *testing.T) {
	reset(t)
	deployClients(t, 2)
	want := pods(t, clientNS, "app="+clientDP)
	for _, tc := range []struct {
		args         []string
		attach, read string
	}{
		{nil, "pidfd_getfd", "process_vm_readv"},
		{[]string{"--force-proc-fallback"}, "proc", "process_vm_readv"},
		{[]string{"--force-mem-fallback"}, "pidfd_getfd", "proc_mem"},
		{[]string{"--force-proc-fallback", "--force-mem-fallback"}, "proc", "proc_mem"},
	} {
		label := tc.attach + " + " + tc.read
		kubectlErr("delete", "namespace", agentNS, "--ignore-not-found", "--wait=true", "--timeout=120s")
		deployAgent(t, append([]string{"--rate=10", "--rescan=2s", "--lease=3s"}, tc.args...)...)
		s := waitSummary(t, 30*time.Second, label, func(s summary) bool {
			if !allAttached(t, want)(s) {
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
		t.Logf("%s: %d clients attached, acking and reporting state", label, len(s.Clients))
	}
}

// S12: missing privileges degrade, never crash. Without CAP_SYS_ADMIN hello
// uprobes are unavailable (mapping events + rescans); without BPF/PERFMON
// discovery is scan-only; without SYS_PTRACE nothing can attach, and clients
// simply run on their defaults.
func TestS12Degraded(t *testing.T) {
	reset(t)
	deployClients(t, 2)
	want := pods(t, clientNS, "app="+clientDP)
	for _, tc := range []struct {
		caps      string
		discovery string
		attaches  bool
	}{
		{`["SYS_PTRACE","BPF","PERFMON"]`, "mmap-only", true},
		{`["SYS_PTRACE"]`, "scan-only", true},
		{`["BPF","PERFMON","SYS_ADMIN"]`, "", false},
	} {
		kubectlErr("delete", "namespace", agentNS, "--ignore-not-found", "--wait=true", "--timeout=120s")
		apply(t, part(manifests(t), agentNS))
		kubectl(t, "-n", agentNS, "patch", "daemonset/"+agentDS, "--type=json", "-p",
			fmt.Sprintf(`[{"op":"replace","path":"/spec/template/spec/containers/0/securityContext/capabilities/add","value":%s}]`, tc.caps))
		kubectl(t, "-n", agentNS, "rollout", "status", "daemonset/"+agentDS, "--timeout=120s")
		if tc.attaches {
			s := waitSummary(t, 30*time.Second, "attached with caps "+tc.caps, allAttached(t, want))
			checkHealthy(t, s)
			if tc.discovery != "" && s.Discovery != tc.discovery {
				// mmap-only is only known once a uprobe was tried.
				t.Errorf("caps %s: discovery %q, want %q", tc.caps, s.Discovery, tc.discovery)
			}
			t.Logf("caps %s: discovery %s, %d clients attached", tc.caps, s.Discovery, s.Attached)
			continue
		}
		time.Sleep(10 * time.Second)
		s, ok := latestSummary(t)
		if !ok {
			t.Fatalf("caps %s: agent prints no summary", tc.caps)
		}
		if s.Attached != 0 {
			t.Errorf("caps %s: %d attached without SYS_PTRACE", tc.caps, s.Attached)
		}
		if n := restarts(t, agentNS, "app="+agentDS); n != 0 {
			t.Errorf("caps %s: agent restarted %d times", tc.caps, n)
		}
		for _, p := range want {
			if ev := clientEvents(t, p); len(ev) == 0 || ev[len(ev)-1].Ev == "exit" {
				t.Errorf("%s not running", p)
			}
		}
		t.Logf("caps %s: agent up (discovery %s), nothing attached, clients on their defaults", tc.caps, s.Discovery)
	}
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}
