//go:build linux

package procctl

import (
	"errors"
	"math"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Grant is one entry of a region's grant table (SPEC §5.2).
type Grant struct {
	Kind     uint32
	ID       [16]byte
	GrantID  uint64 // 0 on Write means "new grant": it gets the write's generation (§9.3)
	Duration uint64 // nanoseconds
}

var (
	// ErrNotOwner: this agent does not own the region (SPEC §8.1).
	ErrNotOwner = errors.New("procctl: not the region owner")
	// ErrPreempted: another writer took over during the write (SPEC §7.4).
	ErrPreempted = errors.New("procctl: preempted by another writer")
	// ErrTooLarge: payload or grants exceed the region's capacity.
	ErrTooLarge = errors.New("procctl: payload too large")
)

// area is the agent's writable view of a region's agent area. All control
// block and payload accesses are word-wise atomics (SPEC §7.5).
type area struct {
	mem   []byte // whole region mapping
	body  []byte // agent area
	slots int
}

func (a *area) u64(off int) *atomic.Uint64 {
	return (*atomic.Uint64)(unsafe.Pointer(&a.body[off]))
}

func (a *area) u32(off int) *atomic.Uint32 {
	return (*atomic.Uint32)(unsafe.Pointer(&a.body[off]))
}

// write publishes grants + payload with the seqlock writer protocol and
// returns the new generation. The caller must own the region.
func (a *area) write(payload []byte, grants []Grant, flags uint32) (uint64, []Grant, error) {
	gt := a.slots * EntrySize
	if len(grants) > a.slots {
		return 0, nil, ErrTooLarge
	}
	if CtrlSize+gt+len(payload) > len(a.body) {
		return 0, nil, ErrTooLarge
	}
	seq := a.u64(cSeq)
	s := seq.Load()
	t := s + 1
	if s&1 == 1 {
		t = s + 2 // adopt a cycle left by a dead writer
	}
	if !seq.CompareAndSwap(s, t) {
		return 0, nil, ErrPreempted
	}
	gen := t + 1

	// Assemble the agent payload, padded to whole words.
	n := gt + len(payload)
	buf := make([]byte, (n+7)&^7)
	out := make([]Grant, len(grants))
	for i, g := range grants {
		if g.GrantID == 0 {
			g.GrantID = gen
		}
		out[i] = g
		e := buf[i*EntrySize:]
		copy(e[eID:], g.ID[:])
		le.PutUint64(e[eGrant:], g.GrantID)
		le.PutUint64(e[eDuration:], g.Duration)
		le.PutUint32(e[eKind:], g.Kind)
	}
	copy(buf[gt:], payload)
	crc := crc32c(buf[:n])

	// Go atomics are sequentially consistent: the CAS above is ordered before
	// these stores, and they are ordered before the final CAS.
	for off := 0; off < len(buf); off += 8 {
		a.u64(CtrlSize + off).Store(le.Uint64(buf[off:]))
	}
	a.u32(cLen).Store(uint32(len(payload)))
	a.u32(cFlags).Store(flags)
	a.u32(cCRC).Store(crc)
	if !seq.CompareAndSwap(t, gen) {
		return 0, nil, ErrPreempted
	}
	a.wake()
	return gen, out, nil
}

// wake issues a shared FUTEX_WAKE on the low 32 bits of seq (SPEC §8.4).
func (a *area) wake() {
	addr := uintptr(unsafe.Pointer(&a.body[cSeq]))
	_, _, _ = unix.Syscall6(unix.SYS_FUTEX, addr, futexWake, math.MaxInt32, 0, 0, 0)
}

// readPayload reads the current generation with the seqlock reader protocol.
// Used by tests and by agents adopting a region's grants (SPEC §9.2 A4).
func (a *area) readPayload() (gen uint64, payload []byte, grants []Grant, err error) {
	gt := a.slots * EntrySize
	for tries := 0; tries < 8; tries++ {
		s1 := a.u64(cSeq).Load()
		if s1&1 == 1 {
			continue
		}
		if s1 == 0 {
			return 0, nil, nil, nil
		}
		n := int(a.u32(cLen).Load())
		want := a.u32(cCRC).Load()
		if CtrlSize+gt+n > len(a.body) {
			continue
		}
		total := gt + n
		buf := make([]byte, (total+7)&^7)
		for off := 0; off < len(buf); off += 8 {
			le.PutUint64(buf[off:], a.u64(CtrlSize+off).Load())
		}
		if a.u64(cSeq).Load() != s1 || crc32c(buf[:total]) != want {
			continue
		}
		for i := 0; i < a.slots; i++ {
			e := buf[i*EntrySize:]
			g := Grant{GrantID: le.Uint64(e[eGrant:])}
			if g.GrantID == 0 {
				continue
			}
			copy(g.ID[:], e[eID:eID+16])
			g.Duration = le.Uint64(e[eDuration:])
			g.Kind = le.Uint32(e[eKind:])
			grants = append(grants, g)
		}
		return s1, buf[gt:total], grants, nil
	}
	return 0, nil, nil, errBusy
}

var errBusy = errors.New("procctl: busy")

// FUTEX_WAKE without FUTEX_PRIVATE_FLAG: the word is in a shared mapping.
const futexWake = 1
