package exclusive

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/parca-dev/procctl"
)

var long = flag.Bool("long", false, "run many more model-test schedules")

// ---- simulated node ----------------------------------------------------------

const tick = 10 * time.Millisecond

type world struct {
	t            *testing.T
	rng          *rand.Rand
	now          time.Time
	res          ResourceID
	procs        []*proc
	nextPID      int
	agentID      int
	agentUp      bool
	lastHB       time.Time
	lease        time.Duration
	trace        []string
	arb          *Arbiter
	policy       RoundRobin
	acquisitions map[int]int // pid → successful acquisitions
}

// proc is one process: its region (agent area), its state block, and the
// holder logic of SPEC §9.1.
type proc struct {
	w   *world
	key procctl.Key
	pid int

	// region (written by the agent)
	owner  int
	gen    uint64
	grants []procctl.Grant

	// process behaviour
	alive, hung bool
	done        chan struct{}
	wants       bool
	evalProb    float64 // how often its control thread runs per tick
	unreadable  float64 // probability a state read fails
	transfer    bool    // transfers a renewed grant instead of releasing
	maxDelay    int     // ticks between reading the grant table and acting on it

	// A real control thread reads the table, then acts on what it read; the
	// table can change in between (the race grants_seen_gen covers).
	snapGen    uint64
	snapGrants []procctl.Grant
	actAt      time.Time
	haveSnap   bool

	// holder logic
	using     uint64 // grant it is using; 0 = none
	acquiring uint64 // announced ACQUIRING, not yet using
	firstSeen map[uint64]time.Time
	released  map[uint64]bool

	// state block
	seq   uint64
	seen  uint64
	entry *procctl.ResourceEntry
}

func (w *world) logf(format string, a ...any) {
	w.trace = append(w.trace, fmt.Sprintf("%6dms ", w.now.Sub(time.Unix(0, 0)).Milliseconds())+fmt.Sprintf(format, a...))
	if len(w.trace) > 200 {
		w.trace = w.trace[len(w.trace)-200:]
	}
}

func (w *world) spawn() *proc {
	w.nextPID++
	p := &proc{
		w: w, key: procctl.Key{Ino: uint64(w.nextPID)}, pid: w.nextPID,
		owner: w.agentID, alive: true, done: make(chan struct{}), wants: true,
		evalProb: 1, firstSeen: map[uint64]time.Time{}, released: map[uint64]bool{}, seq: 2,
	}
	w.procs = append(w.procs, p)
	w.logf("spawn pid %d", p.pid)
	return p
}

func (w *world) crash(p *proc) {
	if !p.alive {
		return
	}
	p.alive = false
	p.using, p.acquiring = 0, 0 // the driver frees everything on exit
	close(p.done)
	w.logf("crash pid %d", p.pid)
}

// acquire starts using the resource and checks the safety property.
func (w *world) acquire(p *proc, g uint64) {
	for _, q := range w.procs {
		if q != p && q.alive && q.using != 0 {
			w.t.Fatalf("SAFETY VIOLATION: pid %d acquires grant %d while pid %d uses grant %d\n%s",
				p.pid, g, q.pid, q.using, strings.Join(w.trace, "\n"))
		}
	}
	p.using = g
	w.acquisitions[p.pid]++
	w.logf("pid %d ACQUIRES grant %d", p.pid, g)
}

func (p *proc) report(state uint32, grant uint64) {
	p.seq += 2
	flags := uint32(0)
	if p.wants {
		flags = procctl.ResFlagWant
	}
	p.entry = &procctl.ResourceEntry{Kind: p.w.res.Kind, ID: p.w.res.ID, GrantID: grant, State: state, Flags: flags}
}

// tickHolder runs the process's control thread once (SPEC §9.1 H1–H6).
func (p *proc) tickHolder() {
	w := p.w
	if !p.alive || p.hung {
		return
	}
	if !p.haveSnap {
		if w.rng.Float64() >= p.evalProb {
			return
		}
		p.snapGen, p.snapGrants = p.gen, append([]procctl.Grant(nil), p.grants...)
		p.actAt = w.now.Add(time.Duration(w.rng.Intn(p.maxDelay+1)) * tick)
		p.haveSnap = true
	}
	if w.now.Before(p.actAt) {
		return
	}
	p.haveSnap = false
	var g *procctl.Grant
	for i := range p.snapGrants {
		if p.snapGrants[i].Kind == w.res.Kind && p.snapGrants[i].ID == w.res.ID {
			g = &p.snapGrants[i]
		}
	}
	leaseOK := w.agentUp && w.now.Sub(w.lastHB) < w.lease
	usable := false
	if g != nil && !p.released[g.GrantID] && leaseOK {
		if _, ok := p.firstSeen[g.GrantID]; !ok {
			p.firstSeen[g.GrantID] = w.now
		}
		usable = w.now.Sub(p.firstSeen[g.GrantID]) < time.Duration(g.Duration)
	}
	seen := p.snapGen
	switch {
	case p.using != 0:
		switch {
		case usable && g.GrantID == p.using:
			p.report(procctl.ResHolding, p.using)
		case usable && p.transfer:
			w.logf("pid %d transfers %d -> %d", p.pid, p.using, g.GrantID)
			p.using = g.GrantID
			p.report(procctl.ResHolding, p.using)
		default:
			w.logf("pid %d releases grant %d", p.pid, p.using)
			p.released[p.using] = true
			p.report(procctl.ResReleased, p.using)
			p.using = 0
		}
	case p.acquiring != 0:
		// A slow acquisition (e.g. CUPTI Start) completes on the grant it
		// announced, without re-reading the table, like the reference loop.
		w.acquire(p, p.acquiring)
		p.report(procctl.ResHolding, p.acquiring)
		p.acquiring = 0
	case usable && p.wants:
		p.report(procctl.ResAcquiring, g.GrantID) // H2: announce before using
		if w.rng.Intn(2) == 0 {
			w.acquire(p, g.GrantID) // fast acquisition, same step
			p.report(procctl.ResHolding, g.GrantID)
		} else {
			p.acquiring = g.GrantID
		}
	default:
		st, id := uint32(procctl.ResIdle), uint64(0)
		if p.entry != nil && !p.entry.MayHold() {
			st, id = p.entry.State, p.entry.GrantID
		}
		p.report(st, id)
	}
	if seen > p.seen {
		p.seen = seen
	}
}

// ---- Target implementation ----------------------------------------------------

func (p *proc) Info() procctl.Info    { return procctl.Info{Key: p.key, HostPID: p.pid} }
func (p *proc) Owned() bool           { return p.owner == p.w.agentID }
func (p *proc) Done() <-chan struct{} { return p.done }

func (p *proc) Grants() ([]procctl.Grant, uint64) {
	return append([]procctl.Grant(nil), p.grants...), p.gen
}

func (p *proc) WriteGrants(gs []procctl.Grant) (uint64, []procctl.Grant, error) {
	if !p.Owned() {
		return 0, nil, procctl.ErrNotOwner
	}
	p.gen += 2
	out := make([]procctl.Grant, len(gs))
	for i, g := range gs {
		if g.GrantID == 0 {
			g.GrantID = p.gen
		}
		out[i] = g
	}
	p.grants = out
	p.w.logf("agent writes pid %d gen %d grants %v", p.pid, p.gen, grantIDs(out))
	return p.gen, out, nil
}

func (p *proc) ReadState(context.Context) (procctl.State, error) {
	if !p.alive {
		return procctl.State{}, procctl.ErrExited
	}
	if p.w.rng.Float64() < p.unreadable {
		return procctl.State{}, errors.New("unreadable")
	}
	st := procctl.State{Seq: p.seq, GrantsSeenGen: p.seen}
	if p.entry != nil {
		st.Resources = []procctl.ResourceEntry{*p.entry}
	}
	return st, nil
}

func grantIDs(gs []procctl.Grant) []uint64 {
	var ids []uint64
	for _, g := range gs {
		ids = append(ids, g.GrantID)
	}
	return ids
}

// ---- driver --------------------------------------------------------------------

func newWorld(t *testing.T, seed int64) *world {
	w := &world{
		t: t, rng: rand.New(rand.NewSource(seed)), now: time.Unix(0, 0),
		res: ResourceID{Kind: 1, ID: [16]byte{0xaa}}, agentID: 1, agentUp: true,
		lease: 300 * time.Millisecond, policy: RoundRobin{Hold: 200 * time.Millisecond, Deadline: 400 * time.Millisecond},
		acquisitions: map[int]int{},
	}
	w.startAgent()
	return w
}

func (w *world) startAgent() {
	w.arb = New(func() []Target {
		var ts []Target
		for _, p := range w.procs {
			ts = append(ts, p)
		}
		return ts
	}, Options{
		Kind: w.res.Kind, Policy: w.policy, Now: func() time.Time { return w.now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	w.lastHB = w.now
}

func (w *world) step() {
	w.now = w.now.Add(tick)
	order := w.rng.Perm(len(w.procs))
	for _, i := range order {
		w.procs[i].tickHolder()
	}
	if w.agentUp {
		w.arb.Step(context.Background())
		w.lastHB = w.now
	}
}

// chaos applies random events with the given intensity (0 = none).
func (w *world) chaos(level float64) {
	r := w.rng.Float64
	alive := 0
	for _, p := range w.procs {
		if p.alive {
			alive++
		}
	}
	if alive < 2 || r() < 0.01*level {
		p := w.spawn()
		p.evalProb = []float64{1, 1, 0.5, 0.1}[w.rng.Intn(4)]
		p.unreadable = []float64{0, 0, 0.2}[w.rng.Intn(3)]
		p.transfer = w.rng.Intn(2) == 0
		p.maxDelay = w.rng.Intn(6)
	}
	for _, p := range w.procs {
		if !p.alive {
			continue
		}
		switch {
		case r() < 0.002*level:
			w.crash(p)
		case !p.hung && r() < 0.002*level:
			p.hung = true
			w.logf("pid %d hangs (using=%d)", p.pid, p.using)
		case p.hung && r() < 0.01:
			p.hung = false
			w.logf("pid %d resumes", p.pid)
		case r() < 0.005*level:
			p.wants = !p.wants
		}
	}
	switch {
	case w.agentUp && r() < 0.003*level:
		w.agentUp = false
		w.logf("AGENT DOWN")
	case !w.agentUp && r() < 0.02:
		// A new agent instance claims every region and adopts their grants.
		w.agentID++
		for _, p := range w.procs {
			p.owner = w.agentID
		}
		w.agentUp = true
		w.startAgent()
		w.logf("AGENT RESTARTED as %d", w.agentID)
	}
}

// ---- tests ---------------------------------------------------------------------

// Never two users, under random schedules with crashes, hangs, slow and
// unreadable processes, interest changes, and agent crashes with adoption.
func TestModelSafety(t *testing.T) {
	seeds, steps := 200, 3000
	if *long {
		seeds, steps = 5000, 10000
	}
	if testing.Short() {
		seeds = 20
	}
	for seed := int64(1); seed <= int64(seeds); seed++ {
		w := newWorld(t, seed)
		for i := 0; i < 3; i++ {
			w.spawn()
		}
		for i := 0; i < steps; i++ {
			w.chaos(1)
			w.step()
		}
	}
}

// With well-behaved processes and a stable agent, everyone who wants the
// resource gets turns, and a lone process keeps it without releases.
func TestModelProgress(t *testing.T) {
	w := newWorld(t, 42)
	for i := 0; i < 4; i++ {
		w.spawn()
	}
	for i := 0; i < 1000; i++ { // 10 s of simulated time, 200 ms turns
		w.step()
	}
	for _, p := range w.procs {
		if n := w.acquisitions[p.pid]; n < 5 {
			t.Errorf("pid %d acquired only %d times in 10s with 4 processes and 200ms turns", p.pid, n)
		}
	}

	// A lone holder is renewed, not rotated: it acquires once.
	w2 := newWorld(t, 7)
	lone := w2.spawn()
	lone.transfer = true
	for i := 0; i < 1000; i++ {
		w2.step()
	}
	if n := w2.acquisitions[lone.pid]; n != 1 {
		t.Errorf("lone holder acquired %d times, want 1 (renewal by transfer)", n)
	}
	if lone.using == 0 {
		t.Error("lone holder lost the resource")
	}
}

// A hung holder blocks the resource; nobody else is granted until it
// resumes (and releases) or dies.
func TestModelHungHolderBlocks(t *testing.T) {
	w := newWorld(t, 3)
	a, b := w.spawn(), w.spawn()
	for i := 0; i < 50 && a.using == 0 && b.using == 0; i++ {
		w.step()
	}
	holder, other := a, b
	if b.using != 0 {
		holder, other = b, a
	}
	if holder.using == 0 {
		t.Fatal("nobody acquired")
	}
	holder.hung = true
	before := w.acquisitions[other.pid]
	for i := 0; i < 500; i++ { // 5 s, many hold periods and grant deadlines
		w.step()
	}
	if w.acquisitions[other.pid] != before {
		t.Fatal("granted to another process while the holder was hung")
	}
	st := w.arb.Snapshot()
	if len(st) != 1 || st[0].BlockedFor == 0 {
		t.Fatalf("resource should be reported blocked: %+v", st)
	}
	w.crash(holder)
	for i := 0; i < 50 && other.using == 0; i++ {
		w.step()
	}
	if other.using == 0 {
		t.Fatal("not granted after the hung holder died")
	}
}
