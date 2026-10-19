//go:build linux

package procctl

import (
	"bytes"
	"errors"
	"math"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	// ErrNotOwner: this agent does not own the region (SPEC §8.1).
	ErrNotOwner = errors.New("procctl: not the region owner")
	// ErrPreempted: another writer took over during the write (SPEC §7.4).
	ErrPreempted = errors.New("procctl: preempted by another writer")
	// ErrTooLarge: payload or grants exceed the region's capacity.
	ErrTooLarge = errors.New("procctl: payload too large")
)

// area is the agent's writable view of a region's agent area. The control
// block is accessed in place through atomics, and the agent payload word by
// word, also with atomics (SPEC §7.6).
type area struct {
	mem   []byte // whole region mapping
	body  []byte // agent area
	slots int
}

// cb overlays the control block on the start of the agent area. The area is
// page-aligned, which satisfies the atomics' alignment.
func (a *area) cb() *ctrlBlock { return (*ctrlBlock)(unsafe.Pointer(&a.body[0])) }

func (a *area) word(off int) *atomic.Uint64 {
	return (*atomic.Uint64)(unsafe.Pointer(&a.body[CtrlSize+off]))
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
	cb := a.cb()
	s := cb.Seq.Load()
	t := s + 1
	if s&1 == 1 {
		t = s + 2 // adopt a cycle left by a dead writer
	}
	if !cb.Seq.CompareAndSwap(s, t) {
		return 0, nil, ErrPreempted
	}
	gen := t + 1

	// Assemble the agent payload, padded to whole words.
	out := make([]Grant, a.slots)
	for i, g := range grants {
		if g.GrantID == 0 {
			g.GrantID = gen
		}
		out[i] = g
	}
	var buf bytes.Buffer
	encode(&buf, out) // empty slots encode as zeros
	buf.Write(payload)
	n := buf.Len()
	buf.Write(make([]byte, (8-n%8)%8))
	b := buf.Bytes()
	crc := crc32c(b[:n])

	// Go atomics are sequentially consistent: the CAS above is ordered before
	// these stores, and they are ordered before the final CAS.
	for off := 0; off < len(b); off += 8 {
		a.word(off).Store(le.Uint64(b[off:]))
	}
	cb.PayloadLen.Store(uint32(len(payload)))
	cb.Flags.Store(flags)
	cb.PayloadCRC32.Store(crc)
	if !cb.Seq.CompareAndSwap(t, gen) {
		return 0, nil, ErrPreempted
	}
	a.wake()
	return gen, out[:len(grants)], nil
}

// FUTEX_WAKE without FUTEX_PRIVATE_FLAG: the word is in a shared mapping.
const futexWake = 1

// wake issues a shared FUTEX_WAKE on the low 32 bits of seq (SPEC §8.4).
func (a *area) wake() {
	addr := uintptr(unsafe.Pointer(&a.cb().Seq))
	_, _, _ = unix.Syscall6(unix.SYS_FUTEX, addr, futexWake, math.MaxInt32, 0, 0, 0)
}

var errBusy = errors.New("procctl: busy")

// readPayload reads the current generation with the seqlock reader protocol.
// Used by agents adopting a region's grants (SPEC §9.2 A4) and by tests.
func (a *area) readPayload() (gen uint64, payload []byte, grants []Grant, err error) {
	gt := a.slots * EntrySize
	cb := a.cb()
	for tries := 0; tries < 8; tries++ {
		s1 := cb.Seq.Load()
		if s1&1 == 1 {
			continue
		}
		if s1 == 0 {
			return 0, nil, nil, nil
		}
		n := int(cb.PayloadLen.Load())
		want := cb.PayloadCRC32.Load()
		if CtrlSize+gt+n > len(a.body) {
			continue
		}
		total := gt + n
		buf := make([]byte, (total+7)&^7)
		for off := 0; off < len(buf); off += 8 {
			le.PutUint64(buf[off:], a.word(off).Load())
		}
		if cb.Seq.Load() != s1 || crc32c(buf[:total]) != want {
			continue
		}
		table := make([]Grant, a.slots)
		if err := decode(buf[:gt], &table); err != nil {
			return 0, nil, nil, err
		}
		for _, g := range table {
			if g.GrantID != 0 {
				grants = append(grants, g)
			}
		}
		return s1, buf[gt:total], grants, nil
	}
	return 0, nil, nil, errBusy
}
