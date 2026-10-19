// Package exclusive arbitrates exclusive resources across processes on a node
// (SPEC §9.2): at most one process may hold a resource, a release is confirmed
// only by the holder's state or its exit, and never by time.
package exclusive

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/parca-dev/procctl"
)

// ResourceID names one exclusive resource.
type ResourceID struct {
	Kind uint32
	ID   [16]byte
}

// Target is one process's channel, as seen by the arbiter. *procctl.Client
// implements it.
type Target interface {
	Info() procctl.Info
	Owned() bool
	Grants() ([]procctl.Grant, uint64)
	WriteGrants([]procctl.Grant) (uint64, []procctl.Grant, error)
	ReadState(context.Context) (procctl.State, error)
	Done() <-chan struct{}
}

// Options configures an Arbiter.
type Options struct {
	Kind            uint32          // resource kind this arbiter manages
	Policy          Policy          // default RoundRobin{}
	Ready           <-chan struct{} // closed after the first full discovery pass (A5)
	PollInterval    time.Duration   // idle state polling; default 250ms
	ConfirmInterval time.Duration   // polling while a release is pending; default 5ms
	BlockedAfter    time.Duration   // report a resource blocked after this; default 2s
	ReadTimeout     time.Duration   // per state read; default 1s
	Now             func() time.Time
	Logger          *slog.Logger
}

// Arbiter issues and revokes grants for one resource kind.
type Arbiter struct {
	opt     Options
	targets func() []Target

	mu      sync.Mutex
	res     map[ResourceID]*resource
	adopted map[procctl.Key]bool
	// barrier[k] is the generation found when this agent first owned k's
	// region. Until k reports grants_seen_gen >= barrier[k], it may still act
	// on a grant table from before this agent (A4).
	barrier map[procctl.Key]uint64
	good    map[procctl.Key]procctl.State // last valid, non-stale state per target
}

type grantRec struct {
	target     Target
	key        procctl.Key
	grantID    uint64
	removedGen uint64 // 0 = still in the target's grant table
	issued     time.Time
}

type resource struct {
	id           ResourceID
	recs         []*grantRec
	lastGranted  map[procctl.Key]time.Time
	blockedSince time.Time
	grants       uint64
	renewals     uint64
}

// New creates an arbiter over the targets returned by targets (typically a
// Registry's clients).
func New(targets func() []Target, opt Options) *Arbiter {
	if opt.Policy == nil {
		opt.Policy = RoundRobin{}
	}
	if opt.PollInterval == 0 {
		opt.PollInterval = 250 * time.Millisecond
	}
	if opt.ConfirmInterval == 0 {
		opt.ConfirmInterval = 5 * time.Millisecond
	}
	if opt.BlockedAfter == 0 {
		opt.BlockedAfter = 2 * time.Second
	}
	if opt.ReadTimeout == 0 {
		opt.ReadTimeout = time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	return &Arbiter{
		opt: opt, targets: targets,
		res:     map[ResourceID]*resource{},
		adopted: map[procctl.Key]bool{},
		barrier: map[procctl.Key]uint64{},
		good:    map[procctl.Key]procctl.State{},
	}
}

// Run steps the arbiter until ctx is done.
func (a *Arbiter) Run(ctx context.Context) error {
	if a.opt.Ready != nil {
		select {
		case <-a.opt.Ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		busy := a.Step(ctx)
		d := a.opt.PollInterval
		if busy {
			d = a.opt.ConfirmInterval
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}

type view struct {
	t      Target
	key    procctl.Key
	owned  bool
	exited bool
	st     procctl.State
	ok     bool // st is a valid, non-stale read from this step
}

func (v *view) entry(r ResourceID) *procctl.ResourceEntry {
	if !v.ok {
		return nil
	}
	for i := range v.st.Resources {
		e := &v.st.Resources[i]
		if e.Kind == r.Kind && e.ID == r.ID {
			return e
		}
	}
	return nil
}

// Step reads every target's state once, confirms releases, and applies the
// policy. It reports whether a release is pending (poll faster).
func (a *Arbiter) Step(ctx context.Context) (busy bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.opt.Now()
	if a.opt.Ready != nil {
		select {
		case <-a.opt.Ready:
		default:
			return false // A5: no decisions before the first full discovery pass
		}
	}

	// 1. Read every target's state.
	views := map[procctl.Key]*view{}
	for _, t := range a.targets() {
		v := &view{t: t, key: t.Info().Key, owned: t.Owned()}
		select {
		case <-t.Done():
			v.exited = true
		default:
			rctx, cancel := context.WithTimeout(ctx, a.opt.ReadTimeout)
			st, err := t.ReadState(rctx)
			cancel()
			v.ok = err == nil && !st.Stale
			if v.ok {
				v.st = st
				a.good[v.key] = st
			}
		}
		views[v.key] = v
	}

	// 2. Adopt grants found in newly owned regions (A4).
	for k, v := range views {
		if !v.owned || v.exited || a.adopted[k] {
			continue
		}
		a.adopted[k] = true
		grants, gen := v.t.Grants()
		if gen > 0 {
			a.barrier[k] = gen
		}
		for _, g := range grants {
			if g.Kind != a.opt.Kind {
				continue
			}
			r := a.resource(ResourceID{g.Kind, g.ID})
			if r.find(k, g.GrantID) == nil {
				r.recs = append(r.recs, &grantRec{target: v.t, key: k, grantID: g.GrantID, issued: now})
				a.opt.Logger.Info("adopted grant", "pid", v.t.Info().HostPID, "grant", g.GrantID)
			}
		}
	}
	for k := range a.adopted {
		if _, ok := views[k]; !ok {
			delete(a.adopted, k)
			delete(a.good, k)
			delete(a.barrier, k)
		}
	}
	for k, g := range a.barrier {
		if v := views[k]; v.exited || (v.ok && v.st.GrantsSeenGen >= g) {
			delete(a.barrier, k)
		}
	}

	// Resources anyone wants or may hold.
	for _, v := range views {
		if !v.ok {
			continue
		}
		for _, e := range v.st.Resources {
			if e.Kind == a.opt.Kind && (e.Flags&procctl.ResFlagWant != 0 || e.MayHold()) {
				a.resource(ResourceID{e.Kind, e.ID})
			}
		}
	}

	type write struct {
		t      Target
		revoke map[ResourceID]bool
		renew  map[ResourceID]bool
		grant  map[ResourceID]bool
	}
	writes := map[procctl.Key]*write{}
	wr := func(v *view) *write {
		w := writes[v.key]
		if w == nil {
			w = &write{t: v.t, revoke: map[ResourceID]bool{}, renew: map[ResourceID]bool{}, grant: map[ResourceID]bool{}}
			writes[v.key] = w
		}
		return w
	}

	for _, r := range a.res {
		// 3. Confirm releases (A2, A3).
		kept := r.recs[:0]
		for _, rec := range r.recs {
			v := views[rec.key]
			if v == nil {
				select {
				case <-rec.target.Done():
					continue // process exited: the resource is free
				default:
					kept = append(kept, rec) // not listed this step; keep waiting
					continue
				}
			}
			if v.exited {
				continue // process exited: the resource is free
			}
			if a.confirmed(r, rec, v) {
				if rec.removedGen == 0 && v.owned {
					wr(v).revoke[r.id] = true // self-released: tidy the table
				}
				continue
			}
			kept = append(kept, rec)
		}
		r.recs = kept

		// 4. Holders we did not grant (another agent's region, or a grant we
		//    could not adopt) block the resource (A5), as does a target whose
		//    state can't be read but last claimed it.
		foreign := false
		for k, v := range views {
			if v.exited {
				continue
			}
			var e *procctl.ResourceEntry
			if v.ok {
				e = v.entry(r.id)
			} else if st, had := a.good[k]; had {
				lv := &view{ok: true, st: st}
				e = lv.entry(r.id)
			}
			if e != nil && e.MayHold() && r.find(k, e.GrantID) == nil {
				foreign = true
			}
			// Not yet caught up with the table this agent found (A4): it may
			// act on an older grant for any resource it lists, or for any
			// resource at all if we have never read its state.
			if _, pending := a.barrier[k]; pending {
				_, had := a.good[k]
				if !had || e != nil {
					foreign = true
				}
			}
		}

		var present, pending []*grantRec
		for _, rec := range r.recs {
			if rec.removedGen == 0 {
				present = append(present, rec)
			} else {
				pending = append(pending, rec)
			}
		}
		if len(pending) > 0 || foreign {
			busy = true
			if r.blockedSince.IsZero() {
				r.blockedSince = now
			}
		} else {
			r.blockedSince = time.Time{}
		}

		// 5. Decide.
		var cands []Candidate
		for _, v := range views {
			if !v.owned || v.exited || !v.ok {
				continue
			}
			if e := v.entry(r.id); e != nil && e.Flags&procctl.ResFlagWant != 0 {
				cands = append(cands, Candidate{Key: v.key, PID: v.t.Info().HostPID, LastGranted: r.lastGranted[v.key]})
			}
		}
		sort.Slice(cands, func(i, j int) bool {
			if !cands[i].LastGranted.Equal(cands[j].LastGranted) {
				return cands[i].LastGranted.Before(cands[j].LastGranted)
			}
			return cands[i].PID < cands[j].PID
		})

		if len(present) > 1 {
			// More than one present grant (only possible after adoption of an
			// inconsistent state): revoke them all and start over.
			for _, rec := range present {
				if v := views[rec.key]; v != nil && v.owned {
					wr(v).revoke[r.id] = true
				}
			}
			continue
		}
		if len(present) == 1 {
			rec := present[0]
			v := views[rec.key]
			if !v.owned {
				continue // we lost the region; its new owner adopts the grant
			}
			e := v.entry(r.id)
			wants := e != nil && e.Flags&procctl.ResFlagWant != 0
			h := Holder{Key: rec.key, PID: v.t.Info().HostPID, Since: rec.issued, Wants: wants}
			switch a.opt.Policy.Decide(r.id, &h, cands, now) {
			case Revoke:
				wr(v).revoke[r.id] = true
			case Renew:
				wr(v).renew[r.id] = true
			}
			continue
		}
		if len(pending) > 0 || foreign || len(cands) == 0 {
			continue
		}
		if a.opt.Policy.Decide(r.id, nil, cands, now) == Grant {
			to := views[cands[0].Key]
			wr(to).grant[r.id] = true
		}
	}

	// 6. Write the changed grant tables, one generation per target.
	for k, w := range writes {
		cur, _ := w.t.Grants()
		var next []procctl.Grant
		for _, g := range cur {
			id := ResourceID{g.Kind, g.ID}
			if g.Kind == a.opt.Kind && (w.revoke[id] || w.renew[id]) {
				if w.renew[id] {
					next = append(next, procctl.Grant{Kind: g.Kind, ID: g.ID, Duration: a.opt.Policy.Duration()})
				}
				continue
			}
			next = append(next, g)
		}
		for id := range w.grant {
			next = append(next, procctl.Grant{Kind: id.Kind, ID: id.ID, Duration: a.opt.Policy.Duration()})
		}
		gen, out, err := w.t.WriteGrants(next)
		if err != nil {
			a.opt.Logger.Warn("write grants", "pid", w.t.Info().HostPID, "err", err)
			continue
		}
		newID := map[ResourceID]uint64{}
		for _, g := range out {
			if g.GrantID == gen {
				newID[ResourceID{g.Kind, g.ID}] = gen
			}
		}
		for id := range w.revoke {
			r := a.res[id]
			for _, rec := range r.recs {
				if rec.key == k && rec.removedGen == 0 {
					rec.removedGen = gen
				}
			}
			busy = true
		}
		for id := range w.renew {
			r := a.res[id]
			for _, rec := range r.recs {
				if rec.key == k && rec.removedGen == 0 {
					rec.removedGen = gen
				}
			}
			r.recs = append(r.recs, &grantRec{target: w.t, key: k, grantID: newID[id], issued: now})
			r.renewals++
			busy = true
		}
		for id := range w.grant {
			r := a.res[id]
			r.recs = append(r.recs, &grantRec{target: w.t, key: k, grantID: newID[id], issued: now})
			r.lastGranted[k] = now
			r.grants++
			a.opt.Logger.Info("granted", "pid", w.t.Info().HostPID, "grant", newID[id])
		}
	}
	return busy
}

// confirmed applies SPEC §9.2 A2 to one outstanding grant.
func (a *Arbiter) confirmed(r *resource, rec *grantRec, v *view) bool {
	if !v.ok {
		return false // A3: unreadable or stale state never confirms
	}
	e := v.entry(r.id)
	if e != nil && e.GrantID == rec.grantID &&
		(e.State == procctl.ResReleased || e.State == procctl.ResFailed) {
		return true
	}
	if rec.removedGen == 0 || v.st.GrantsSeenGen < rec.removedGen {
		return false
	}
	if e == nil || !e.MayHold() {
		return true
	}
	// Renewal by transfer: the holder now holds a newer grant we issued.
	return e.GrantID != rec.grantID && r.find(rec.key, e.GrantID) != nil
}

func (a *Arbiter) resource(id ResourceID) *resource {
	r := a.res[id]
	if r == nil {
		r = &resource{id: id, lastGranted: map[procctl.Key]time.Time{}}
		a.res[id] = r
	}
	return r
}

func (r *resource) find(k procctl.Key, grantID uint64) *grantRec {
	for _, rec := range r.recs {
		if rec.key == k && rec.grantID == grantID {
			return rec
		}
	}
	return nil
}

// Status describes one resource for metrics and debugging.
type Status struct {
	ID         ResourceID
	HolderPID  int // 0 = none
	GrantID    uint64
	HeldFor    time.Duration
	Pending    int           // grants awaiting confirmed release
	BlockedFor time.Duration // > 0 while blocked (pending release or foreign holder)
	Grants     uint64
	Renewals   uint64
}

// Snapshot returns the status of every known resource.
func (a *Arbiter) Snapshot() []Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.opt.Now()
	var out []Status
	for _, r := range a.res {
		s := Status{ID: r.id, Grants: r.grants, Renewals: r.renewals}
		for _, rec := range r.recs {
			if rec.removedGen == 0 {
				s.HolderPID = rec.target.Info().HostPID
				s.GrantID = rec.grantID
				s.HeldFor = now.Sub(rec.issued)
			} else {
				s.Pending++
			}
		}
		if !r.blockedSince.IsZero() && now.Sub(r.blockedSince) >= a.opt.BlockedAfter {
			s.BlockedFor = now.Sub(r.blockedSince)
		}
		out = append(out, s)
	}
	return out
}
