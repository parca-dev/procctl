package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	agentNS  = "procctl-system"
	clientNS = "procctl-clients"
	agentDS  = "procctl-testagent"
	clientDP = "procctl-testclient"
)

// ---- kubectl ----------------------------------------------------------------

func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectlErr(args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func kubectlErr(args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return b.String(), err
}

// manifests renders the deploy/ overlay with the images under test.
func manifests(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("kubectl", "kustomize", filepath.Join("..", "deploy", "overlays", "kind")).Output()
	if err != nil {
		t.Fatalf("kustomize: %v", err)
	}
	s := string(out)
	if *imageRegistry != "" {
		s = strings.ReplaceAll(s, "image: procctl-testagent:dev", fmt.Sprintf("image: %s/procctl-testagent:%s", *imageRegistry, *imageTag))
		s = strings.ReplaceAll(s, "image: procctl-testclient:dev", fmt.Sprintf("image: %s/procctl-testclient:%s", *imageRegistry, *imageTag))
	}
	return s
}

// part returns the documents of a rendered manifest that mention namespace ns.
func part(all, ns string) string {
	var keep []string
	for _, doc := range strings.Split(all, "\n---\n") {
		isNS := regexp.MustCompile(`(?m)^kind: Namespace$`).MatchString(doc) &&
			regexp.MustCompile(`(?m)^  name: `+regexp.QuoteMeta(ns)+`$`).MatchString(doc)
		if isNS || strings.Contains(doc, "namespace: "+ns) {
			if isNS {
				keep = append([]string{doc}, keep...) // namespaces first
			} else {
				keep = append(keep, doc)
			}
		}
	}
	return strings.Join(keep, "\n---\n")
}

func apply(t *testing.T, yaml string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(f, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	kubectl(t, "apply", "-f", f)
}

// reset removes everything from a previous scenario.
func reset(t *testing.T) {
	t.Helper()
	kubectlErr("delete", "namespace", agentNS, clientNS, "--ignore-not-found", "--wait=true", "--timeout=120s")
}

func deployClients(t *testing.T, replicas int, args ...string) {
	t.Helper()
	apply(t, part(manifests(t), clientNS))
	if len(args) > 0 {
		setArgs(t, clientNS, "deployment/"+clientDP, args)
	}
	kubectl(t, "-n", clientNS, "scale", "deployment/"+clientDP, fmt.Sprintf("--replicas=%d", replicas))
	kubectl(t, "-n", clientNS, "rollout", "status", "deployment/"+clientDP, "--timeout=180s")
}

func deployAgent(t *testing.T, args ...string) {
	t.Helper()
	apply(t, part(manifests(t), agentNS))
	if os.Getenv("E2E_AGENT_DEBUG") != "" {
		args = append(args, "--debug")
	}
	if len(args) > 0 {
		setArgs(t, agentNS, "daemonset/"+agentDS, args)
	}
	kubectl(t, "-n", agentNS, "rollout", "status", "daemonset/"+agentDS, "--timeout=120s")
}

func setArgs(t *testing.T, ns, obj string, args []string) {
	t.Helper()
	b, _ := json.Marshal(args)
	patch := fmt.Sprintf(`[{"op":"replace","path":"/spec/template/spec/containers/0/args","value":%s}]`, b)
	kubectl(t, "-n", ns, "patch", obj, "--type=json", "-p", patch)
}

func pods(t *testing.T, ns, label string) []string {
	t.Helper()
	out := kubectl(t, "-n", ns, "get", "pods", "-l", label, "--field-selector=status.phase=Running", "-o", "json")
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("pods: %v", err)
	}
	var names []string
	for _, it := range list.Items {
		if it.Metadata.DeletionTimestamp == nil { // terminating pods are still Running
			names = append(names, it.Metadata.Name)
		}
	}
	return names
}

// ---- agent summary ---------------------------------------------------------------

type clientSummary struct {
	Pod          string  `json:"pod"`
	HostPID      int     `json:"host_pid"`
	NSPid        []int   `json:"ns_pid"`
	StatusNSPid  int     `json:"status_ns_pid"`
	StartTime    uint64  `json:"start_time"`
	HostStart    uint64  `json:"host_start_time"`
	Owned        bool    `json:"owned"`
	Path         string  `json:"path"`
	ReadPath     string  `json:"read_path"`
	OwnChanges   int     `json:"own_changes"`
	WakeP50us    float64 `json:"wake_p50_us"`
	WakeP99us    float64 `json:"wake_p99_us"`
	WakeSamples  int     `json:"wake_samples"`
	WakeFails    int     `json:"wake_fails"`
	Stale        bool    `json:"stale"`
	AckedGen     uint64  `json:"acked_gen"`
	LastTag      uint64  `json:"last_tag"`
	Torn         uint64  `json:"torn"`
	Backwards    uint64  `json:"backwards"`
	Redelivered  uint64  `json:"redelivered"`
	Changes      uint64  `json:"changes"`
	LatP50us     float64 `json:"lat_p50_us"`
	LatP99us     float64 `json:"lat_p99_us"`
	Samples      int     `json:"samples"`
	LeaseExpired bool    `json:"lease_expired"`
	Holding      bool    `json:"holding"`
	Acquires     uint64  `json:"acquires"`
	Releases     uint64  `json:"releases"`
	WriteErrs    uint64  `json:"write_errs"`
	ReadErrs     uint64  `json:"read_errs"`
}

type summary struct {
	TS        time.Time      `json:"ts"`
	Node      string         `json:"node"`
	AgentPID  int            `json:"agent_pid"`
	Kernel    string         `json:"kernel"`
	UptimeS   float64        `json:"uptime_s"`
	Attached  int            `json:"attached"`
	Owned     int            `json:"owned"`
	Detached  int            `json:"detached"`
	Rejected  map[string]int `json:"rejected"`
	RSSKB     int64          `json:"rss_kb"`
	Mapped    int            `json:"mapped_regions"`
	CPUS      float64        `json:"cpu_s"`
	Discovery string         `json:"discovery"`
	Scan      struct {
		Procs int     `json:"procs"`
		FDs   int     `json:"fds"`
		MS    float64 `json:"ms"`
		Count int     `json:"count"`
	} `json:"scan"`
	Hello *struct {
		FilesInspected, FilesWithHello, Uprobes, UprobesActive, FilesCached, UprobeErrors, Firings, MappingEvents, LostEvents, InspectFailures uint64
	} `json:"hello"`
	Clients   []clientSummary `json:"clients"`
	Resources []struct {
		HolderPID int     `json:"holder_pid"`
		Pending   int     `json:"pending"`
		BlockedMS float64 `json:"blocked_ms"`
		Grants    uint64  `json:"grants"`
		Renewals  uint64  `json:"renewals"`
	} `json:"resources"`
}

// latestSummary returns the newest summary line of the agent pod(s).
func latestSummary(t *testing.T) (summary, bool) { return latestSummaryOf(t, agentDS) }

// latestSummaryOf returns the newest summary of the agent DaemonSet app.
func latestSummaryOf(t *testing.T, app string) (summary, bool) {
	t.Helper()
	var best summary
	found := false
	for _, p := range pods(t, agentNS, "app="+app) {
		out, err := kubectlErr("-n", agentNS, "logs", p, "--tail=50")
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(strings.NewReader(out))
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "PROCCTL_SUMMARY ") {
				continue
			}
			var s summary
			if json.Unmarshal([]byte(line[len("PROCCTL_SUMMARY "):]), &s) == nil && s.TS.After(best.TS) {
				best, found = s, true
			}
		}
	}
	return best, found
}

// waitSummary polls until pred holds for the latest summary.
func waitSummary(t *testing.T, timeout time.Duration, what string, pred func(summary) bool) summary {
	t.Helper()
	return waitSummaryOf(t, agentDS, timeout, what, pred)
}

func waitSummaryOf(t *testing.T, app string, timeout time.Duration, what string, pred func(summary) bool) summary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last summary
	for time.Now().Before(deadline) {
		if s, ok := latestSummaryOf(t, app); ok {
			last = s
			if pred(s) {
				return s
			}
		}
		time.Sleep(time.Second)
	}
	b, _ := json.MarshalIndent(last, "", "  ")
	t.Fatalf("timed out after %v waiting for %s (%s); last summary:\n%s", timeout, what, app, b)
	return last
}

func byPod(s summary) map[string]clientSummary {
	m := map[string]clientSummary{}
	for _, c := range s.Clients {
		if c.Pod != "" {
			m[c.Pod] = c
		}
	}
	return m
}

// allAttached: every running client pod is attached, owned and acking.
func allAttached(t *testing.T, want []string) func(summary) bool {
	return func(s summary) bool {
		m := byPod(s)
		for _, p := range want {
			c, ok := m[p]
			if !ok || !c.Owned || c.AckedGen == 0 || c.Samples == 0 {
				return false
			}
		}
		return true
	}
}

// checkHealthy asserts invariants that must hold in every scenario:
// no torn payloads, no generation going backwards, and (S15) the agent's pid
// mapping points at the very process the client says it is.
func checkHealthy(t *testing.T, s summary) {
	t.Helper()
	for _, c := range s.Clients {
		if c.Torn != 0 || c.Backwards != 0 {
			t.Errorf("%s: torn=%d backwards=%d", c.Pod, c.Torn, c.Backwards)
		}
		if c.Stale || c.Pod == "" {
			continue
		}
		if len(c.NSPid) == 0 || c.NSPid[len(c.NSPid)-1] != c.StatusNSPid {
			t.Errorf("S15 %s: agent maps host pid %d to ns pids %v, client reports getpid()=%d",
				c.Pod, c.HostPID, c.NSPid, c.StatusNSPid)
		}
		if c.HostStart != c.StartTime {
			t.Errorf("S15 %s: start time differs: host view %d, client view %d", c.Pod, c.HostStart, c.StartTime)
		}
	}
	if s.Mapped != s.Attached {
		t.Errorf("agent maps %d regions for %d attached clients", s.Mapped, s.Attached)
	}
}

func logLatencies(t *testing.T, s summary) {
	t.Helper()
	var p50, p99 []float64
	for _, c := range s.Clients {
		if c.Samples > 0 {
			p50 = append(p50, c.LatP50us)
			p99 = append(p99, c.LatP99us)
		}
	}
	sort.Float64s(p50)
	sort.Float64s(p99)
	if len(p50) > 0 {
		t.Logf("%d clients; per-client latency p50 median %.0fµs, worst p99 %.0fµs; agent rss %d KiB, cpu %.2fs",
			len(p50), p50[len(p50)/2], p99[len(p99)-1], s.RSSKB, s.CPUS)
	}
}

// ---- client events (pod logs) ------------------------------------------------------

type clientEvent struct {
	Ev           string  `json:"ev"`
	Pod          string  `json:"pod"`
	PID          int     `json:"pid"`
	TS           uint64  `json:"ts"`
	Expired      *int    `json:"expired"`
	Grant        uint64  `json:"grant"`
	SinceStartMS float64 `json:"since_start_ms"`
	Reinit       *int    `json:"reinit"`
	InheritedAck int     `json:"inherited_ack"`
	Mode         string  `json:"mode"`
	Total        uint64  `json:"total"`
}

func clientEvents(t *testing.T, pod string) []clientEvent {
	t.Helper()
	out, err := kubectlErr("-n", clientNS, "logs", pod)
	if err != nil {
		t.Fatalf("logs %s: %v", pod, err)
	}
	var evs []clientEvent
	for _, line := range strings.Split(out, "\n") {
		var e clientEvent
		if json.Unmarshal([]byte(line), &e) == nil && e.Ev != "" {
			evs = append(evs, e)
		}
	}
	return evs
}

// allClientEvents also includes the previous container's log (a client
// killed and restarted in place).
func allClientEvents(t *testing.T, pod string) []clientEvent {
	t.Helper()
	var evs []clientEvent
	for _, prev := range []bool{true, false} {
		args := []string{"-n", clientNS, "logs", pod}
		if prev {
			args = append(args, "--previous")
		}
		out, err := kubectlErr(args...)
		if err != nil {
			if prev {
				continue
			}
			t.Fatalf("logs %s: %v", pod, err)
		}
		for _, line := range strings.Split(out, "\n") {
			var e clientEvent
			if json.Unmarshal([]byte(line), &e) == nil && e.Ev != "" {
				evs = append(evs, e)
			}
		}
	}
	return evs
}

func leaseEvents(t *testing.T, pod string, expired bool) []clientEvent {
	var out []clientEvent
	for _, e := range clientEvents(t, pod) {
		if e.Ev == "lease" && e.Expired != nil && (*e.Expired == 1) == expired {
			out = append(out, e)
		}
	}
	return out
}

// ---- node access (kind only) ---------------------------------------------------------

// nodeExec runs a command on the kind node, as root.
func nodeExec(t *testing.T, node string, args ...string) string {
	t.Helper()
	if !onKind {
		t.Skip("needs node access (kind)")
	}
	out, err := exec.Command("docker", append([]string{"exec", node}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %s %v: %v\n%s", node, args, err, out)
	}
	return string(out)
}

// ---- variants -------------------------------------------------------------------

// variantDoc renders the base client Deployment (or agent DaemonSet) under
// another name and app label.
func variantDoc(t *testing.T, ns, kind, base, name string) string {
	t.Helper()
	for _, doc := range strings.Split(part(manifests(t), ns), "\n---\n") {
		if strings.Contains(doc, "kind: "+kind+"\n") {
			doc = strings.ReplaceAll(doc, "name: "+base+"\n", "name: "+name+"\n")
			return strings.ReplaceAll(doc, "app: "+base+"\n", "app: "+name+"\n")
		}
	}
	t.Fatalf("no %s in the %s manifests", kind, ns)
	return ""
}

func nsDoc(t *testing.T, ns string) string {
	t.Helper()
	return strings.Split(part(manifests(t), ns), "\n---\n")[0]
}

// deployVariant runs replicas of the test client as a Deployment of its own,
// with args and optional strategic-merge patches. It returns the running pods,
// or the error if the rollout fails.
func deployVariant(t *testing.T, name string, replicas int, args []string, patches ...string) ([]string, error) {
	t.Helper()
	return deployVariantWithin(t, 180*time.Second, name, replicas, args, patches...)
}

func deployVariantWithin(t *testing.T, timeout time.Duration, name string, replicas int, args []string, patches ...string) ([]string, error) {
	t.Helper()
	apply(t, nsDoc(t, clientNS))
	// Created with no replicas, so nothing runs before args and patches apply.
	doc := regexp.MustCompile(`(?m)^  replicas: \d+$`).ReplaceAllString(variantDoc(t, clientNS, "Deployment", clientDP, name), "  replicas: 0")
	apply(t, doc)
	obj := "deployment/" + name
	setArgs(t, clientNS, obj, args)
	for _, p := range patches {
		kubectl(t, "-n", clientNS, "patch", obj, "-p", p)
	}
	kubectl(t, "-n", clientNS, "scale", obj, fmt.Sprintf("--replicas=%d", replicas))
	if out, err := kubectlErr("-n", clientNS, "rollout", "status", obj, fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))); err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	return pods(t, clientNS, "app="+name), nil
}

// deployAgentVariant runs another agent DaemonSet (S13).
func deployAgentVariant(t *testing.T, name string, args ...string) {
	t.Helper()
	apply(t, variantDoc(t, agentNS, "DaemonSet", agentDS, name))
	if len(args) > 0 {
		setArgs(t, agentNS, "daemonset/"+name, args)
	}
	kubectl(t, "-n", agentNS, "rollout", "status", "daemonset/"+name, "--timeout=120s")
}

// restarts sums the container restarts of the pods matching label.
func restarts(t *testing.T, ns, label string) int {
	t.Helper()
	out := kubectl(t, "-n", ns, "get", "pods", "-l", label, "-o",
		"jsonpath={range .items[*]}{.status.containerStatuses[*].restartCount}{\" \"}{end}")
	n := 0
	for _, f := range strings.Fields(out) {
		v, _ := strconv.Atoi(f)
		n += v
	}
	return n
}

// sameProcess reports whether pid on the node is still the process that
// started at start (field 22 of /proc/<pid>/stat), not a reused pid.
func sameProcess(node string, pid int, start uint64) bool {
	out, err := exec.Command("docker", "exec", node, "cat", fmt.Sprintf("/proc/%d/stat", pid)).Output()
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(out, ')')
	if i < 0 {
		return false
	}
	f := strings.Fields(string(out[i+1:]))
	return len(f) >= 20 && f[19] == strconv.FormatUint(start, 10)
}

// signal sends sig to a host pid on the kind node.
func signal(t *testing.T, node string, sig string, pid int) {
	t.Helper()
	nodeExec(t, node, "kill", "-"+sig, strconv.Itoa(pid))
}

// signalClient signals the process the agent attached for pod, after checking
// it is still that process (the pod may have restarted since the summary).
func signalClient(t *testing.T, s summary, pod, sig string) {
	t.Helper()
	c, ok := byPod(s)[pod]
	if !ok || !sameProcess(s.Node, c.HostPID, c.HostStart) {
		t.Fatalf("%s: process %d from the summary is gone", pod, c.HostPID)
	}
	signal(t, s.Node, sig, c.HostPID)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
