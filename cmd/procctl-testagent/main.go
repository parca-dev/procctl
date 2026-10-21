//go:build linux

// procctl-testagent: the agent side of the e2e tests.
//
// Finds the test clients on its node, pushes a self-checking payload to each
// at --rate, reads their status, and prints a summary line every
// --summary-interval:
//
//	PROCCTL_SUMMARY {"node":...,"clients":[...],...}
//
// The e2e suite reads it from the pod log.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
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
)

var (
	consumer     = flag.String("consumer", consumerName, "consumer to serve (each e2e scenario uses its own)")
	rate         = flag.Float64("rate", 10, "payload writes per second, per client")
	payloadBytes = flag.Int("payload-bytes", 4096, "pattern bytes per payload")
	rescan       = flag.Duration("rescan", 2*time.Second, "discovery rescan interval")
	lease        = flag.Duration("lease", 3*time.Second, "lease written into each region")
	readEvery    = flag.Duration("read-interval", 250*time.Millisecond, "how often to read each client's status")
	summaryEvery = flag.Duration("summary-interval", 500*time.Millisecond, "how often to print the summary")
	release      = flag.Bool("release", true, "on exit, mark regions RELEASED; false simulates a crash")
)

const latencyWindow = 4096

// tracked is what the agent knows about one client.
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
	ownFlips  int
}

type agent struct {
	reg      *procctl.Registry
	log      *slog.Logger
	mu       sync.Mutex
	clients  map[procctl.Key]*tracked
	rejected map[string]int
	detached int
	start    time.Time
}

func main() {
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	a := &agent{log: log, clients: map[procctl.Key]*tracked{}, rejected: map[string]int{}, start: time.Now()}
	reg, err := procctl.NewRegistry(procctl.RegistryOptions{
		Consumer: *consumer, RescanInterval: *rescan, Lease: *lease, Logger: log,
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

	writeTick := time.NewTicker(time.Duration(float64(time.Second) / *rate))
	readTick := time.NewTicker(*readEvery)
	sumTick := time.NewTicker(*summaryEvery)
	for {
		select {
		case <-ctx.Done():
			a.summary()
			if *release {
				_ = reg.Close()
			}
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

// reasonOf shortens a rejection to its cause, for counting.
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
		a.clients[info.Key] = &tracked{c: ev.Client}
	case procctl.Detached:
		delete(a.clients, info.Key)
		a.detached++
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
	h := payloadHeader{Seq_tag: tag, Agent_write_ns: bootNS(), Pattern_len: uint32(*payloadBytes)}
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, &h)
	for i := 0; i < *payloadBytes; i++ {
		x := tag*0x9e3779b97f4a7c15 + uint64(i) // testclient_pattern()
		x ^= x >> 31
		x *= 0xbf58476d1ce4e5b9
		b.WriteByte(byte(x >> 56))
	}
	return b.Bytes()
}

func (a *agent) writeAll() {
	for _, t := range a.snapshot() {
		if !t.c.Owned() {
			continue
		}
		t.tag++
		if _, err := t.c.Write(payload(t.tag)); err != nil {
			t.writeErrs++
		}
	}
}

func (a *agent) readAll(ctx context.Context) {
	for _, t := range a.snapshot() {
		st, err := t.c.ReadState(ctx)
		a.mu.Lock()
		if o := t.c.Owned(); !t.ownSeen || o != t.owned {
			if t.ownSeen {
				t.ownFlips++
			}
			t.owned, t.ownSeen = o, true
		}
		if err != nil {
			t.readErrs++
			a.mu.Unlock()
			continue
		}
		t.stale, t.ackedGen = st.Stale, st.AppliedGen
		var cs clientStatus
		if !st.Stale && len(st.Consumer) >= int(unsafe.Sizeof(cs)) &&
			binary.Read(bytes.NewReader(st.Consumer), binary.LittleEndian, &cs) == nil {
			if cs.Last_seq_tag != t.lastTag && cs.Last_agent_write_ns != 0 && cs.Last_recv_ns >= cs.Last_agent_write_ns {
				t.lastTag = cs.Last_seq_tag
				d := time.Duration(cs.Last_recv_ns - cs.Last_agent_write_ns)
				if len(t.latencies) < latencyWindow {
					t.latencies = append(t.latencies, d)
				} else {
					t.latencies[t.latNext] = d
					t.latNext = (t.latNext + 1) % latencyWindow
				}
			}
			t.status, t.haveState = cs, true
		}
		a.mu.Unlock()
	}
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
	Mapped   int             `json:"mapped_regions"` // procctl memfds mapped in this agent
	Clients  []clientSummary `json:"clients"`
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
		Kernel: unix.ByteSliceToString(u.Release[:]), RSSKB: ru.Maxrss,
		CPUS: time.Duration(ru.Utime.Nano() + ru.Stime.Nano()).Seconds(),
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
			HostPID: info.HostPID, NSPid: info.NSPid, Owned: t.c.Owned(), OwnChanges: t.ownFlips,
			Stale: t.stale, AckedGen: t.ackedGen, WriteErrs: t.writeErrs, ReadErrs: t.readErrs,
			LatP50us: pct(t.latencies, 0.50), LatP99us: pct(t.latencies, 0.99), Samples: len(t.latencies),
			HostStart: hostStartTime(info.HostPID),
		}
		if t.haveState {
			st := t.status
			cs.Pod = unix.ByteSliceToString(st.Pod[:])
			cs.StatusNSPid, cs.StartTime = st.Ns_pid, st.Start_time
			cs.LastTag, cs.Torn, cs.Backwards = st.Last_seq_tag, st.Torn, st.Backwards
			cs.Redelivered, cs.Changes = st.Redelivered, st.Changes
			cs.LeaseExpired = st.Lease_expired != 0
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
	b, _ := json.Marshal(s)
	fmt.Printf("PROCCTL_SUMMARY %s\n", b)
}

// hostStartTime is field 22 of /proc/<pid>/stat in the agent's (host) pid
// namespace, to check the agent's pid mapping against the client's own view.
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

// mappedRegions counts the procctl regions mapped into this agent: a detached
// region must be unmapped.
func mappedRegions() int {
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return -1
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "/memfd:procctl:") {
			if f := strings.Fields(l); len(f) >= 5 {
				seen[f[4]] = true // one region = one inode
			}
		}
	}
	return len(seen)
}
