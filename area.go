//go:build linux

package procctl

import (
	"errors"
	"sync/atomic"
	"unsafe"
)

var (
	// ErrNotOwner: this agent does not own the region.
	ErrNotOwner = errors.New("procctl: not the region owner")
	// ErrPreempted: another agent wrote the region during this write.
	ErrPreempted = errors.New("procctl: preempted by another writer")
	// ErrTooLarge: the payload exceeds the region's capacity.
	ErrTooLarge = errors.New("procctl: payload too large")
	errBusy     = errors.New("procctl: busy")
)

// area is the agent's writable view of a region's agent area: the control
// block, then the payload. Both are shared with the process, so every access
// is atomic (the payload word by word).
type area struct{ body []byte }

// cb overlays the control block on the page-aligned start of the agent area.
func (a area) cb() *ctrlBlock { return (*ctrlBlock)(unsafe.Pointer(&a.body[0])) }

func (a area) word(off int) *atomic.Uint64 {
	return (*atomic.Uint64)(unsafe.Pointer(&a.body[CtrlSize+off]))
}

func ld64(p *uint64) uint64                 { return atomic.LoadUint64(p) }
func ld32(p *uint32) uint32                 { return atomic.LoadUint32(p) }
func st64(p *uint64, v uint64)              { atomic.StoreUint64(p, v) }
func st32(p *uint32, v uint32)              { atomic.StoreUint32(p, v) }
func cas64(p *uint64, old, new uint64) bool { return atomic.CompareAndSwapUint64(p, old, new) }

// write publishes payload as a new generation (seqlock writer) and returns it.
// Both seq transitions are CASes, so two agents writing at once can't both
// succeed; an odd seq left by a dead writer is taken over (s+2 stays odd).
func (a area) write(payload []byte) (uint64, error) {
	if CtrlSize+len(payload) > len(a.body) {
		return 0, ErrTooLarge
	}
	cb := a.cb()
	s := ld64(&cb.Seq)
	t := s + 1
	if s&1 == 1 {
		t = s + 2
	}
	if !cas64(&cb.Seq, s, t) {
		return 0, ErrPreempted
	}
	padded := make([]byte, (len(payload)+7)&^7)
	copy(padded, payload)
	for off := 0; off < len(padded); off += 8 {
		a.word(off).Store(le.Uint64(padded[off:]))
	}
	st32(&cb.Payload_len, uint32(len(payload)))
	st32(&cb.Payload_crc, crc32c(payload))
	if !cas64(&cb.Seq, t, t+1) {
		return 0, ErrPreempted
	}
	return t + 1, nil
}

// read returns the current generation and payload (seqlock reader); gen 0 if
// the region was never written.
func (a area) read() (uint64, []byte, error) {
	cb := a.cb()
	for tries := 0; tries < 8; tries++ {
		s := ld64(&cb.Seq)
		if s == 0 {
			return 0, nil, nil
		}
		n := int(ld32(&cb.Payload_len))
		if s&1 == 1 || CtrlSize+n > len(a.body) {
			continue
		}
		crc := ld32(&cb.Payload_crc)
		buf := make([]byte, (n+7)&^7)
		for off := 0; off < len(buf); off += 8 {
			le.PutUint64(buf[off:], a.word(off).Load())
		}
		if ld64(&cb.Seq) == s && crc32c(buf[:n]) == crc {
			return s, buf[:n], nil
		}
	}
	return 0, nil, errBusy
}
