package e2e

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Object names inside each scenario's namespaces.
const (
	agentDS  = "procctl-testagent"
	clientDP = "procctl-testclient"
)

// long selects the plan's full sizes and durations (nightly); the default is
// the fast PR suite.
var long = os.Getenv("E2E_LONG") != ""

// runID keeps this run's namespaces apart from leftovers of earlier ones.
var runID = func() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

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
	return kubectlIn(nil, args...)
}

func kubectlIn(stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	return b.String(), err
}

// ---- manifests -----------------------------------------------------------------

type baseManifests struct {
	agentNS, clientNS corev1.Namespace
	agent             appsv1.DaemonSet
	client            appsv1.Deployment
}

var (
	baseOnce sync.Once
	base     baseManifests
	baseErr  error
)

// manifests renders deploy/overlays/kind once and decodes its objects.
func manifests(t *testing.T) baseManifests {
	t.Helper()
	baseOnce.Do(func() {
		out, err := exec.Command("kubectl", "kustomize", filepath.Join("..", "deploy", "overlays", "kind")).Output()
		if err != nil {
			baseErr = fmt.Errorf("kustomize: %w", err)
			return
		}
		for _, doc := range strings.Split(string(out), "\n---\n") {
			var meta struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
				baseErr = err
				return
			}
			switch {
			case meta.Kind == "Namespace" && meta.Metadata.Name == "procctl-system":
				baseErr = yaml.Unmarshal([]byte(doc), &base.agentNS)
			case meta.Kind == "Namespace":
				baseErr = yaml.Unmarshal([]byte(doc), &base.clientNS)
			case meta.Kind == "DaemonSet":
				baseErr = yaml.Unmarshal([]byte(doc), &base.agent)
			case meta.Kind == "Deployment":
				baseErr = yaml.Unmarshal([]byte(doc), &base.client)
			}
			if baseErr != nil {
				return
			}
		}
	})
	if baseErr != nil {
		t.Fatal(baseErr)
	}
	return base
}

func image(name string) string {
	if *imageRegistry != "" {
		return fmt.Sprintf("%s/%s:%s", *imageRegistry, name, *imageTag)
	}
	return name + ":dev"
}

func applyObj(t *testing.T, obj any) {
	t.Helper()
	b, err := yaml.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := kubectlIn(b, "apply", "-f", "-"); err != nil {
		t.Fatalf("kubectl apply: %v\n%s\n%s", err, out, b)
	}
}

// ---- per-scenario environment ----------------------------------------------------

// scenario is one scenario's slice of the cluster: its own agent and client
// namespaces and its own consumer name. An agent only serves its consumer, so
// scenarios run in parallel on one node without seeing each other's clients.
type scenario struct {
	t                 *testing.T
	agentNS, clientNS string
	consumer          string
}

// newScenario creates the namespaces; they are deleted when the test ends
// (kept on failure with E2E_KEEP_FAILED, for debugging; not by default, since a
// failed scenario's load would otherwise disturb the ones still running).
func newScenario(t *testing.T, id string) *scenario {
	t.Helper()
	m := manifests(t)
	e := &scenario{
		t:        t,
		agentNS:  "pa-" + id + "-" + runID,
		clientNS: "pc-" + id + "-" + runID,
		consumer: "tc_" + strings.ReplaceAll(id, "-", "_"),
	}
	if len(e.consumer) > 31 {
		t.Fatalf("consumer %q too long", e.consumer)
	}
	for _, ns := range []struct {
		obj  corev1.Namespace
		name string
	}{{m.agentNS, e.agentNS}, {m.clientNS, e.clientNS}} {
		obj := *ns.obj.DeepCopy()
		obj.Name = ns.name
		if obj.Labels == nil {
			obj.Labels = map[string]string{}
		}
		obj.Labels["procctl-e2e"] = "1"
		applyObj(t, &obj)
	}
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("E2E_KEEP_FAILED") != "" {
			t.Logf("keeping namespaces %s and %s", e.agentNS, e.clientNS)
			return
		}
		kubectlErr("delete", "namespace", e.agentNS, e.clientNS, "--wait=false")
	})
	return e
}

// agentArgs: defaults, then the scenario's, then the consumer (last wins).
func (e *scenario) agentArgs(args []string) []string {
	a := append([]string{"--rate=10", "--rescan=2s", "--lease=3s"}, args...)
	return append(a, "--consumer="+e.consumer)
}

// agent deploys the agent DaemonSet named name (agentDS by default) and waits
// for it. mutate, if set, edits the object before it is applied.
func (e *scenario) agent(name string, mutate func(*appsv1.DaemonSet), args ...string) {
	e.t.Helper()
	m := manifests(e.t)
	ds := m.agent.DeepCopy()
	ds.Namespace, ds.Name = e.agentNS, name
	ds.Labels = map[string]string{"app": name}
	ds.Spec.Selector.MatchLabels = map[string]string{"app": name}
	ds.Spec.Template.Labels = map[string]string{"app": name}
	c := &ds.Spec.Template.Spec.Containers[0]
	c.Image, c.Args = image("procctl-testagent"), e.agentArgs(args)
	if mutate != nil {
		mutate(ds)
	}
	applyObj(e.t, ds)
	kubectl(e.t, "-n", e.agentNS, "rollout", "status", "daemonset/"+name, "--timeout=120s")
}

// clients deploys replicas of the test client as Deployment name and returns
// the running pods.
func (e *scenario) clients(name string, replicas int, args ...string) []string {
	e.t.Helper()
	ps, err := e.clientsWithin(180*time.Second, name, replicas, nil, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	return ps
}

func (e *scenario) clientsWithin(timeout time.Duration, name string, replicas int, mutate func(*appsv1.Deployment), args ...string) ([]string, error) {
	e.t.Helper()
	m := manifests(e.t)
	d := m.client.DeepCopy()
	d.Namespace, d.Name = e.clientNS, name
	d.Labels = map[string]string{"app": name}
	d.Spec.Selector.MatchLabels = map[string]string{"app": name}
	d.Spec.Template.Labels = map[string]string{"app": name}
	r := int32(replicas)
	d.Spec.Replicas = &r
	c := &d.Spec.Template.Spec.Containers[0]
	c.Image = image("procctl-testclient")
	c.Args = append(append([]string{"--poll-us=5000"}, args...), "--consumer="+e.consumer)
	if mutate != nil {
		mutate(d)
	}
	applyObj(e.t, d)
	if out, err := kubectlErr("-n", e.clientNS, "rollout", "status", "deployment/"+name,
		fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))); err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	return e.pods(e.clientNS, "app="+name), nil
}

func (e *scenario) pods(ns, label string) []string {
	e.t.Helper()
	out := kubectl(e.t, "-n", ns, "get", "pods", "-l", label, "--field-selector=status.phase=Running", "-o", "json")
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		e.t.Fatalf("pods: %v", err)
	}
	var names []string
	for _, it := range list.Items {
		if it.Metadata.DeletionTimestamp == nil { // terminating pods are still Running
			names = append(names, it.Metadata.Name)
		}
	}
	return names
}

// restarts sums the container restarts of the pods matching label in ns.
func (e *scenario) restarts(ns, label string) int {
	e.t.Helper()
	out := kubectl(e.t, "-n", ns, "get", "pods", "-l", label, "-o",
		"jsonpath={range .items[*]}{.status.containerStatuses[*].restartCount}{\" \"}{end}")
	n := 0
	for _, f := range strings.Fields(out) {
		v, _ := strconv.Atoi(f)
		n += v
	}
	return n
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
	OwnChanges   int     `json:"own_changes"`
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
	WriteErrs    uint64  `json:"write_errs"`
	ReadErrs     uint64  `json:"read_errs"`
}

type summary struct {
	TS       time.Time       `json:"ts"`
	Node     string          `json:"node"`
	AgentPID int             `json:"agent_pid"`
	Kernel   string          `json:"kernel"`
	Attached int             `json:"attached"`
	Owned    int             `json:"owned"`
	Detached int             `json:"detached"`
	Rejected map[string]int  `json:"rejected"`
	RSSKB    int64           `json:"rss_kb"`
	CPUS     float64         `json:"cpu_s"`
	Mapped   int             `json:"mapped_regions"`
	Clients  []clientSummary `json:"clients"`
}

// latest returns the newest summary of the scenario's agent.
func (e *scenario) latest() (summary, bool) { return e.latestOf(agentDS) }

// latestOf returns the newest summary of the agent DaemonSet app.
func (e *scenario) latestOf(app string) (summary, bool) {
	e.t.Helper()
	var best summary
	found := false
	for _, p := range e.pods(e.agentNS, "app="+app) {
		out, err := kubectlErr("-n", e.agentNS, "logs", p, "--tail=20")
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

// wait polls until pred holds for the latest summary.
func (e *scenario) wait(timeout time.Duration, what string, pred func(summary) bool) summary {
	e.t.Helper()
	return e.waitOf(agentDS, timeout, what, pred)
}

func (e *scenario) waitOf(app string, timeout time.Duration, what string, pred func(summary) bool) summary {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	var last summary
	for time.Now().Before(deadline) {
		if s, ok := e.latestOf(app); ok {
			last = s
			if pred(s) {
				return s
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(last, "", "  ")
	e.t.Fatalf("timed out after %v waiting for %s (%s); last summary:\n%s", timeout, what, app, b)
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

// allAttached: every listed client pod is attached, owned and acking.
func allAttached(want []string) func(summary) bool {
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
	SinceStartMS float64 `json:"since_start_ms"`
	Reinit       *int    `json:"reinit"`
	InheritedAck int     `json:"inherited_ack"`
}

func parseEvents(out string) []clientEvent {
	var evs []clientEvent
	for _, line := range strings.Split(out, "\n") {
		var e clientEvent
		if json.Unmarshal([]byte(line), &e) == nil && e.Ev != "" {
			evs = append(evs, e)
		}
	}
	return evs
}

// events returns the client pod's log events (current container).
func (e *scenario) events(pod string) []clientEvent {
	e.t.Helper()
	out, err := kubectlErr("-n", e.clientNS, "logs", pod)
	if err != nil {
		e.t.Fatalf("logs %s: %v", pod, err)
	}
	return parseEvents(out)
}

func (e *scenario) leaseEvents(pod string, expired bool) []clientEvent {
	var out []clientEvent
	for _, ev := range e.events(pod) {
		if ev.Ev == "lease" && ev.Expired != nil && (*ev.Expired == 1) == expired {
			out = append(out, ev)
		}
	}
	return out
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
