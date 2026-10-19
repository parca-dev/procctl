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
// page-aligned and the fields are naturally aligned, as the atomics need.
func (a *area) cb() *ctrlBlock { return (*ctrlBlock)(unsafe.Pointer(&a.body[0])) }

// The control block is shared with the process: every access is atomic.
func ld64(p *uint64) uint64                 { return atomic.LoadUint64(p) }
func ld32(p *uint32) uint32                 { return atomic.LoadUint32(p) }
func st64(p *uint64, v uint64)              { atomic.StoreUint64(p, v) }
func st32(p *uint32, v uint32)              { atomic.StoreUint32(p, v) }
func cas64(p *uint64, old, new uint64) bool { return atomic.CompareAndSwapUint64(p, old, new) }

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
	s := ld64(&cb.Seq)
	t := s + 1
	if s&1 == 1 {
		t = s + 2 // adopt a cycle left by a dead writer
	}
	if !cas64(&cb.Seq, s, t) {
		return 0, nil, ErrPreempted
	}
	gen := t + 1

	// Assemble the agent payload, padded to whole words.
	out := make([]Grant, len(grants))
	table := make([]grantEntry, a.slots) // empty slots encode as zeros
	for i, g := range grants {
		if g.GrantID == 0 {
			g.GrantID = gen
		}
		out[i] = g
		table[i] = g.wire()
	}
	var buf bytes.Buffer
	encode(&buf, table)
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
	st32(&cb.Payload_len, uint32(len(payload)))
	st32(&cb.Flags, flags)
	st32(&cb.Payload_crc32c, crc)
	if !cas64(&cb.Seq, t, gen) {
		return 0, nil, ErrPreempted
	}
	a.wake()
	return gen, out, nil
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
		s1 := ld64(&cb.Seq)
		if s1&1 == 1 {
			continue
		}
		if s1 == 0 {
			return 0, nil, nil, nil
		}
		n := int(ld32(&cb.Payload_len))
		want := ld32(&cb.Payload_crc32c)
		if CtrlSize+gt+n > len(a.body) {
			continue
		}
		total := gt + n
		buf := make([]byte, (total+7)&^7)
		for off := 0; off < len(buf); off += 8 {
			le.PutUint64(buf[off:], a.word(off).Load())
		}
		if ld64(&cb.Seq) != s1 || crc32c(buf[:total]) != want {
			continue
		}
		table := make([]grantEntry, a.slots)
		if err := decode(buf[:gt], &table); err != nil {
			return 0, nil, nil, err
		}
		for _, e := range table {
			if e.Grant_id != 0 {
				grants = append(grants, grantFromWire(e))
			}
		}
		return s1, buf[gt:total], grants, nil
	}
	return 0, nil, nil, errBusy
}
