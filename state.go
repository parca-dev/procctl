//go:build linux

package procctl

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// MayHold reports whether the entry claims the resource may be in use.
func (e ResourceEntry) MayHold() bool {
	return e.State == ResAcquiring || e.State == ResHolding
}

// State is a validated snapshot of a process's state block (SPEC §6).
type State struct {
	Seq             uint64
	AppliedGen      uint64
	AppliedWriterID uint64
	GrantsSeenGen   uint64
	WriterPID       uint32
	// Stale: the block was written by another pid (e.g. inherited across
	// fork; SPEC §12). A stale block reports no state and no holdings.
	Stale     bool
	Resources []ResourceEntry
	Consumer  []byte
}

var (
	// ErrStateBusy: the state block kept changing during reads.
	ErrStateBusy = errors.New("procctl: state block busy")
	// ErrStateInvalid: the state block failed validation.
	ErrStateInvalid = errors.New("procctl: state block invalid")
	// ErrTimeout: a remote read did not complete in time (SPEC §13).
	ErrTimeout = errors.New("procctl: remote read timed out")
)

// remoteReader reads another process's memory (SPEC §7.3).
type remoteReader interface {
	read(addr uint64, dst []byte) error
}

// vmReader uses process_vm_readv.
type vmReader struct{ pid int }

func (r vmReader) read(addr uint64, dst []byte) error {
	local := []unix.Iovec{{Base: &dst[0]}}
	local[0].SetLen(len(dst))
	remote := []unix.RemoteIovec{{Base: uintptr(addr), Len: len(dst)}}
	n, err := unix.ProcessVMReadv(r.pid, local, remote, 0)
	if err != nil {
		return err
	}
	if n != len(dst) {
		return unix.EFAULT
	}
	return nil
}

// memReader preads /proc/<pid>/mem. The fd is bound to the process's memory
// when opened, so it can't follow a reused pid.
type memReader struct{ f *os.File }

func (r memReader) read(addr uint64, dst []byte) error {
	if addr > math.MaxInt64 {
		return unix.EFAULT
	}
	n, err := r.f.ReadAt(dst, int64(addr))
	if n == len(dst) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = unix.EFAULT
	}
	return err
}

func remoteReadU64(r remoteReader, addr uint64) (uint64, error) {
	var v uint64
	b := unsafe.Slice((*byte)(unsafe.Pointer(&v)), 8)
	if err := r.read(addr, b); err != nil {
		return 0, err
	}
	return le.Uint64(b), nil
}

// readStateOnce performs the SPEC §7.3 procedure once.
func readStateOnce(r remoteReader, h *Header, innermostNSPid int) (State, error) {
	var st State
	seqAddr := h.StateAddr + uint64(stateSeqOff)
	s1, err := remoteReadU64(r, seqAddr)
	if err != nil {
		return st, err
	}
	if s1&1 == 1 || s1 == 0 {
		return st, ErrStateBusy
	}
	buf := make([]byte, h.StateSize)
	if err := r.read(h.StateAddr, buf); err != nil {
		return st, err
	}
	s2, err := remoteReadU64(r, seqAddr)
	if err != nil {
		return st, err
	}
	if s1 != s2 || le.Uint64(buf[stateSeqOff:]) != s1 {
		return st, ErrStateBusy
	}
	return parseState(buf, h, innermostNSPid)
}

// parseState validates and decodes a state block copied out of a process.
// It is pure, so it can be fuzzed and tested against golden vectors.
func parseState(buf []byte, h *Header, innermostNSPid int) (State, error) {
	var st State
	var sh stateHeader
	if len(buf) < StateHdrSize || len(buf) > MaxStateSize || decode(buf, &sh) != nil {
		return st, ErrStateInvalid
	}
	if sh.Seq&1 == 1 || sh.Seq == 0 {
		return st, ErrStateBusy
	}
	if string(sh.Magic[:]) != stateMagic || sh.Instance_id != h.InstanceID {
		return st, ErrStateInvalid
	}
	tableEnd := StateHdrSize + int(h.ResourceSlots)*resEntrySize
	used := tableEnd + int(sh.State_len)
	if used > len(buf) {
		return st, ErrStateInvalid
	}
	// CRC over the used block with seq and crc32c taken as zero (SPEC §6).
	chk := append([]byte(nil), buf[:used]...)
	clear(chk[stateSeqOff : stateSeqOff+8])
	clear(chk[stateCRCOff : stateCRCOff+4])
	if crc32c(chk) != sh.Crc32c {
		return st, ErrStateBusy // torn across the syscalls, or corrupt
	}
	st = State{
		Seq: sh.Seq, WriterPID: sh.Writer_pid, AppliedGen: sh.Applied_gen,
		AppliedWriterID: sh.Applied_writer_id, GrantsSeenGen: sh.Grants_seen_gen,
		Stale: int(sh.Writer_pid) != innermostNSPid,
	}
	if st.Stale {
		return st, nil
	}
	table := make([]resEntry, h.ResourceSlots)
	if err := decode(buf[StateHdrSize:tableEnd], &table); err != nil {
		return State{}, ErrStateInvalid
	}
	for _, r := range table {
		if r.Kind != 0 || r.Resource_id != ([16]byte{}) {
			st.Resources = append(st.Resources, resourceFromWire(r))
		}
	}
	st.Consumer = append([]byte(nil), buf[tableEnd:used]...)
	return st, nil
}

// readState retries readStateOnce under a deadline. A read that is still
// blocked when ctx expires is abandoned (its goroutine finishes on its own).
func readState(ctx context.Context, rd remoteReader, h *Header, innermost int, timeout time.Duration) (State, error) {
	if _, ok := ctx.Deadline(); !ok && timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	type result struct {
		st  State
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var r result
		for tries := 0; tries < 8; tries++ {
			r.st, r.err = readStateOnce(rd, h, innermost)
			if !errors.Is(r.err, ErrStateBusy) {
				break
			}
		}
		ch <- r
	}()
	select {
	case r := <-ch:
		return r.st, r.err
	case <-ctx.Done():
		return State{}, ErrTimeout
	}
}
