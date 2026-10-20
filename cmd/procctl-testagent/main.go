//go:build linux

// procctl-testagent: the agent side of the e2e tests.
//
// Discovers procctl-testclient processes on the node, pushes self-checking
// payloads to each at --rate, reads their status blocks, and (with
// --exclusive) arbitrates the simulated node-wide token resource.
//
// It prints a machine-readable summary line every --summary-interval (and on
// exit) to stdout:
//
//	PROCCTL_SUMMARY {"node":...,"clients":[...],...}
//
// The e2e suite reads it from the pod logs.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/parca-dev/procctl"
	"github.com/parca-dev/procctl/exclusive"
	"github.com/parca-dev/procctl/hello"
)

var (
	rate          = flag.Float64("rate", 10, "payload writes per second, per client")
	payloadBytes  = flag.Int("payload-bytes", 4096, "pattern bytes per payload")
	rescan        = flag.Duration("rescan", 2*time.Second, "discovery rescan interval")
	lease         = flag.Duration("lease", 3*time.Second, "lease written into each region")
	readInterval  = flag.Duration("read-interval", 250*time.Millisecond, "how often to read each client's status")
	summaryEvery  = flag.Duration("summary-interval", time.Second, "how often to print the summary")
	useExclusive  = flag.Bool("exclusive", false, "arbitrate the token resource")
	hold          = flag.Duration("hold", 2*time.Second, "exclusive: per-turn hold time")
	deadline      = flag.Duration("deadline", 6*time.Second, "exclusive: grant duration")
	procFallback  = flag.Bool("force-proc-fallback", false, "attach via /proc/<pid>/fd instead of pidfd_getfd")
	memFallback   = flag.Bool("force-mem-fallback", false, "read state via /proc/<pid>/mem instead of process_vm_readv")
	wakeProbe     = flag.Duration("wake-probe", 0, "if > 0 (and not --exclusive): this often, rewrite each --exclusive client's grant table and time until its state shows grants_seen_gen (S16)")
	uninstall     = flag.Bool("uninstall", false, "on exit, clear ownership so clients revert immediately")
	debug         = flag.Bool("debug", false, "debug logging")
	useHello      = flag.Bool("hello", true, "fast discovery: hello-probe uprobes and mapping events (falls back to scan-only)")
	latencyWindow = 4096
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// tracked is everything the agent knows about one client.
type tracked struct {
	c         *procctl.Client
	tag       uint64
	writeErrs uint64
	readErrs  uint64
	lastTag   uint64
	latencies []time.Duration // ring buffer
	latNext   int
	status    clientStatus
	haveState bool
	stale     bool
	ackedGen  uint64
	owned     bool
	ownSeen   bool
	ownFlips  int             // ownership changes after attach (S13)
	wake      []time.Duration // --wake-probe latencies (ring buffer)
	wakeNext  int
	wakeFails int
}

type agent struct {
	reg      *procctl.Registry
	arb      *exclusive.Arbiter
	log      *slog.Logger
	mu       sync.Mutex
	clients  map[procctl.Key]*tracked
	rejected map[string]int
	detached int
	start    time.Time
	hello    *hello.Watcher
	mode     string // discovery: "hello" or "scan-only"
}

func main() {
	flag.Parse()
	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	a := &agent{log: log, clients: map[procctl.Key]*tracked{}, rejected: map[string]int{}, start: time.Now()}
	reg, err := procctl.NewRegistry(procctl.RegistryOptions{
		Consumer: consumerName, RescanInterval: *rescan, Lease: *lease, Logger: log,
		ForceProcFallback: *procFallback, ForceMemFallback: *memFallback,
		OnReject: func(pid, fd int, err error) {
			a.mu.Lock()
			a.rejected[reasonOf(err)]++
			a.mu.Unlock()
		},
	})
	if err != nil {
		log.Error("registry", "err", err)
		os.Exit(1)
	}
	a.reg = reg
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { _ = reg.Run(ctx) }()
	a.mode = "scan-only"
	if *useHello {
		w, err := hello.New(hello.Options{Notify: reg.Notify, Logger: log})
		if err != nil {
			log.Warn("fast discovery unavailable, scan-only", "err", err)
		} else {
			a.hello, a.mode = w, "hello"
			go func() { _ = w.Run(ctx) }()
			defer w.Close()
		}
	}
	if *useExclusive {
		a.arb = exclusive.New(func() []exclusive.Target {
			var ts []exclusive.Target
			for _, c := range reg.Clients() {
				ts = append(ts, c)
			}
			return ts
		}, exclusive.Options{
			Kind: kindToken, Policy: exclusive.RoundRobin{Hold: *hold, Deadline: *deadline},
			Ready: reg.FirstScanDone(), Logger: log, BlockedAfter: time.Second,
		})
		go func() { _ = a.arb.Run(ctx) }()
	}

	if *wakeProbe > 0 && !*useExclusive {
		go a.wakeLoop(ctx)
	}
	writeTick := time.NewTicker(time.Duration(float64(time.Second) / *rate))
	readTick := time.NewTicker(*readInterval)
	sumTick := time.NewTicker(*summaryEvery)
	defer writeTick.Stop()
	defer readTick.Stop()
	defer sumTick.Stop()
	for {
		select {
		case <-ctx.Done():
			a.summary()
			var opts []procctl.CloseOption
			if *uninstall {
				opts = append(opts, procctl.ReleaseLeases(true))
			}
			_ = reg.Close(opts...)
			return
		case ev := <-reg.Events():
			a.event(ev)
		case <-writeTick.C:
			a.writeAll()
		case <-readTick.C:
			a.readAll(ctx)
		case <-sumTick.C:
			a.summary()
		}
	}
}

func reasonOf(err error) string {
	s := err.Error()
	if i := strings.Index(s, "invalid region: "); i >= 0 {
		s = s[i+len("invalid region: "):]
	}
	if i := strings.IndexAny(s, "(0123456789"); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func (a *agent) event(ev procctl.Event) {
	info := ev.Client.Info()
	a.mu.Lock()
	defer a.mu.Unlock()
	switch ev.Kind {
	case procctl.Attached:
		a.clients[info.Key] = &tracked{c: ev.Client, latencies: make([]time.Duration, 0, latencyWindow)}
		a.log.Info("attached", "host_pid", info.HostPID, "ns_pid", info.NSPid, "path", info.AttachPath,
			"owned", ev.Client.Owned())
	case procctl.Detached:
		delete(a.clients, info.Key)
		a.detached++
		a.log.Info("detached", "host_pid", info.HostPID, "reason", ev.Reason)
	}
}

func (a *agent) snapshot() []*tracked {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*tracked, 0, len(a.clients))
	for _, t := range a.clients {
		out = append(out, t)
	}
	return out
}

func bootNS() uint64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return uint64(ts.Nano())
}

// payload builds a self-checking payload (testclient.h).
func payload(tag uint64) []byte {
	pat := make([]byte, *payloadBytes)
	for i := range pat {
		pat[i] = pattern(tag, uint32(i))
	}
	h := payloadHeader{Seq_tag: tag, Agent_write_ns: bootNS(), Pattern_len: uint32(len(pat)),
		Pattern_crc32c: crc32.Checksum(pat, castagnoli)}
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, &h)
	b.Write(pat)
	return b.Bytes()
}

// pattern mirrors testclient_pattern() in testclient.h.
func pattern(tag uint64, i uint32) byte {
	x := tag*0x9e3779b97f4a7c15 + uint64(i)
	x ^= x >> 31
	x *= 0xbf58476d1ce4e5b9
	return byte(x >> 56)
}

func (a *agent) writeAll() {
	for _, t := range a.snapshot() {
		if !t.c.Owned() {
			continue
		}
		t.tag++
		if _, err := t.c.WritePayload(payload(t.tag)); err != nil {
			t.writeErrs++
		}
	}
}

func (a *agent) readAll(ctx context.Context) {
	for _, t := range a.snapshot() {
		st, err := t.c.ReadState(ctx)
		a.mu.Lock()
		if err != nil {
			t.readErrs++
			a.mu.Unlock()
			continue
		}
		t.stale = st.Stale
		t.ackedGen = st.AppliedGen
		if o := t.c.Owned(); !t.ownSeen || o != t.owned {
			if t.ownSeen {
				t.ownFlips++
			}
			t.owned, t.ownSeen = o, true
		}
		if !st.Stale && len(st.Consumer) >= int(unsafe.Sizeof(clientStatus{})) {
			var cs clientStatus
			if binary.Read(bytes.NewReader(st.Consumer), binary.LittleEndian, &cs) == nil {
				if cs.Last_seq_tag != t.lastTag && cs.Last_recv_ns >= cs.Last_agent_write_ns && cs.Last_agent_write_ns != 0 {
					t.lastTag = cs.Last_seq_tag
					d := time.Duration(cs.Last_recv_ns - cs.Last_agent_write_ns)
					if len(t.latencies) < latencyWindow {
						t.latencies = append(t.latencies, d)
					} else {
						t.latencies[t.latNext] = d
						t.latNext = (t.latNext + 1) % latencyWindow
					}
				}
				t.status = cs
				t.haveState = true
			}
		}
		a.mu.Unlock()
	}
}

// wakeLoop measures S16: agent write → the client's control thread wakes
// (futex on the read-only mapping) → its state shows grants_seen_gen.
func (a *agent) wakeLoop(ctx context.Context) {
	tick := time.NewTicker(*wakeProbe)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, t := range a.snapshot() {
			a.mu.Lock()
			excl := t.haveState && t.status.Mode&modeExclusive != 0
			a.mu.Unlock()
			if !excl || !t.c.Owned() {
				continue
			}
			d, ok := measureWake(ctx, t.c)
			a.mu.Lock()
			if !ok {
				t.wakeFails++
			} else if len(t.wake) < latencyWindow {
				t.wake = append(t.wake, d)
			} else {
				t.wake[t.wakeNext] = d
				t.wakeNext = (t.wakeNext + 1) % latencyWindow
			}
			a.mu.Unlock()
		}
	}
}

func measureWake(ctx context.Context, c *procctl.Client) (time.Duration, bool) {
	gen, _, err := c.WriteGrants(nil)
	if err != nil {
		return 0, false
	}
	start := time.Now()
	for time.Since(start) < 500*time.Millisecond {
		st, err := c.ReadState(ctx)
		if err == nil && !st.Stale && st.GrantsSeenGen >= gen {
			return time.Since(start), true
		}
		time.Sleep(200 * time.Microsecond)
	}
	return 0, false
}

// ---- summary -------------------------------------------------------------------

type clientSummary struct {
	Pod          string  `json:"pod"`
	HostPID      int     `json:"host_pid"`
	NSPid        []int   `json:"ns_pid"`
	StatusNSPid  int32   `json:"status_ns_pid"`
	StartTime    uint64  `json:"start_time"`      // as the client sees itself
	HostStart    uint64  `json:"host_start_time"` // /proc/<host_pid>/stat as the agent sees it
	Owned        bool    `json:"owned"`
	Path         string  `json:"path"`
	ReadPath     string  `json:"read_path"`
	OwnFlips     int     `json:"own_changes"`
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

type scanSummary struct {
	Procs int     `json:"procs"`
	FDs   int     `json:"fds"`
	MS    float64 `json:"ms"`
	Count int     `json:"count"`
}

type resourceSummary struct {
	HolderPID int     `json:"holder_pid"`
	Pending   int     `json:"pending"`
	BlockedMS float64 `json:"blocked_ms"`
	Grants    uint64  `json:"grants"`
	Renewals  uint64  `json:"renewals"`
}

type summary struct {
	TS        time.Time         `json:"ts"`
	Node      string            `json:"node"`
	AgentPID  int               `json:"agent_pid"`
	Kernel    string            `json:"kernel"`
	UptimeS   float64           `json:"uptime_s"`
	Attached  int               `json:"attached"`
	Owned     int               `json:"owned"`
	Detached  int               `json:"detached"`
	Rejected  map[string]int    `json:"rejected"`
	RSSKB     int64             `json:"rss_kb"`
	Mapped    int               `json:"mapped_regions"` // procctl memfds mapped in this agent
	CPUS      float64           `json:"cpu_s"`
	Discovery string            `json:"discovery"`
	Scan      scanSummary       `json:"scan"`
	Hello     *hello.Stats      `json:"hello,omitempty"`
	Clients   []clientSummary   `json:"clients"`
	Resources []resourceSummary `json:"resources,omitempty"`
}

func pct(ds []time.Duration, p float64) float64 {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return float64(s[int(p*float64(len(s)-1))].Microseconds())
}

func (a *agent) summary() {
	var u unix.Utsname
	_ = unix.Uname(&u)
	var ru unix.Rusage
	_ = unix.Getrusage(unix.RUSAGE_SELF, &ru)
	s := summary{
		TS: time.Now(), Node: os.Getenv("NODE_NAME"), AgentPID: os.Getpid(),
		Kernel:    unix.ByteSliceToString(u.Release[:]),
		UptimeS:   time.Since(a.start).Seconds(),
		RSSKB:     ru.Maxrss,
		CPUS:      time.Duration(ru.Utime.Nano() + ru.Stime.Nano()).Seconds(),
		Discovery: a.mode,
	}
	if st, n := a.reg.LastScan(); n > 0 {
		s.Scan = scanSummary{Procs: st.Procs, FDs: st.FDs, MS: float64(st.Duration.Microseconds()) / 1000, Count: n}
	}
	if a.hello != nil {
		st := a.hello.Stats()
		s.Hello = &st
		if st.Uprobes == 0 && st.UprobeErrors > 0 {
			s.Discovery = "mmap-only" // no CAP_SYS_ADMIN: mapping events and rescans
		}
	}
	a.mu.Lock()
	s.Detached = a.detached
	s.Rejected = map[string]int{}
	for k, v := range a.rejected {
		s.Rejected[k] = v
	}
	for _, t := range a.clients {
		info := t.c.Info()
		cs := clientSummary{
			HostPID: info.HostPID, NSPid: info.NSPid, Owned: t.c.Owned(), Path: info.AttachPath,
			ReadPath: info.ReadPath, OwnFlips: t.ownFlips, WakeFails: t.wakeFails,
			WakeP50us: pct(t.wake, 0.50), WakeP99us: pct(t.wake, 0.99), WakeSamples: len(t.wake),
			Stale: t.stale, AckedGen: t.ackedGen, WriteErrs: t.writeErrs, ReadErrs: t.readErrs,
			LatP50us: pct(t.latencies, 0.50), LatP99us: pct(t.latencies, 0.99), Samples: len(t.latencies),
			HostStart: hostStartTime(info.HostPID),
		}
		if t.haveState {
			st := t.status
			cs.Pod = unix.ByteSliceToString(st.Pod[:])
			cs.StatusNSPid = st.Ns_pid
			cs.StartTime = st.Start_time
			cs.LastTag = st.Last_seq_tag
			cs.Torn = st.Torn
			cs.Backwards = st.Backwards
			cs.Redelivered = st.Redelivered
			cs.Changes = st.Changes
			cs.LeaseExpired = st.Lease_expired != 0
			cs.Holding = st.Holding != 0
			cs.Acquires = st.Acquires
			cs.Releases = st.Releases
		}
		if cs.Owned {
			s.Owned++
		}
		s.Clients = append(s.Clients, cs)
	}
	a.mu.Unlock()
	s.Attached = len(s.Clients)
	s.Mapped = mappedRegions()
	sort.Slice(s.Clients, func(i, j int) bool { return s.Clients[i].Pod < s.Clients[j].Pod })
	if a.arb != nil {
		for _, r := range a.arb.Snapshot() {
			s.Resources = append(s.Resources, resourceSummary{
				HolderPID: r.HolderPID, Pending: r.Pending, BlockedMS: float64(r.BlockedFor.Milliseconds()),
				Grants: r.Grants, Renewals: r.Renewals,
			})
		}
	}
	b, _ := json.Marshal(s)
	fmt.Printf("PROCCTL_SUMMARY %s\n", b)
}

// hostStartTime is field 22 of /proc/<pid>/stat in the agent's (host) pid
// namespace, to cross-check the client's own view of itself (scenario S15).
func hostStartTime(pid int) uint64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0
	}
	var v uint64
	fmt.Sscan(f[19], &v)
	return v
}

// mappedRegions counts procctl regions mapped into this agent (scenario S4:
// detached regions must be unmapped).
func mappedRegions() int {
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return -1
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if i := strings.Index(l, "/memfd:procctl:"); i >= 0 {
			f := strings.Fields(l)
			if len(f) >= 5 {
				seen[f[4]] = true // one region = one inode
			}
		}
	}
	return len(seen)
}
