package exclusive

import (
	"time"

	"github.com/parca-dev/procctl"
)

// Decision is what a Policy wants done with a resource this step.
type Decision int

const (
	Keep   Decision = iota // leave things as they are
	Renew                  // replace the holder's grant with a fresh one (no release)
	Revoke                 // take the grant away from the holder
	Grant                  // grant to the first candidate (only when there is no holder)
)

// Holder describes the process holding a resource.
type Holder struct {
	Key   procctl.Key
	PID   int
	Since time.Time // when its current grant was issued
	Wants bool      // it still lists the resource with WANT
}

// Candidate is a process that wants a resource, in grant order (least
// recently granted first).
type Candidate struct {
	Key         procctl.Key
	PID         int
	LastGranted time.Time
}

// Policy decides who holds what and for how long. Safety (SPEC §9.2 A1–A6)
// is enforced by the Arbiter whatever the policy decides.
type Policy interface {
	// Decide is called with h == nil when nobody holds the resource and no
	// release is pending; cands is never empty then.
	Decide(r ResourceID, h *Holder, cands []Candidate, now time.Time) Decision
	// Duration is the duration_ns written into grants: how long a holder may
	// keep the resource if this agent disappears. It must exceed the policy's
	// own hold time.
	Duration() uint64
}

// RoundRobin rotates a resource among the processes that want it.
type RoundRobin struct {
	Hold     time.Duration // per turn; default 10s
	Deadline time.Duration // grant duration_ns; default Hold + 5s
}

func (p RoundRobin) hold() time.Duration {
	if p.Hold == 0 {
		return 10 * time.Second
	}
	return p.Hold
}

// Duration implements Policy.
func (p RoundRobin) Duration() uint64 {
	if p.Deadline != 0 {
		return uint64(p.Deadline)
	}
	return uint64(p.hold() + 5*time.Second)
}

// Decide implements Policy.
func (p RoundRobin) Decide(r ResourceID, h *Holder, cands []Candidate, now time.Time) Decision {
	if h == nil {
		return Grant
	}
	if !h.Wants {
		return Revoke
	}
	if now.Sub(h.Since) < p.hold() {
		return Keep
	}
	for _, c := range cands {
		if c.Key != h.Key {
			return Revoke // someone else's turn
		}
	}
	return Renew // alone: extend without a hand-off
}
