//go:build linux

package procctl

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Resource states (SPEC §6.1).
const (
	ResIdle      = 0
	ResAcquiring = 1
	ResHolding   = 2
	ResReleased  = 3
	ResFailed    = 4
	ResFlagWant  = 1 << 0
)

// ResourceEntry is one entry of a process's resource table (SPEC §6.1).
type ResourceEntry struct {
	Kind           uint32
	ID             [16]byte
	GrantID        uint64
	State          uint32
	Flags          uint32
	Error          uint32
	LastActivityNS uint64
}

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
	// ErrNoState: the region has no state block.
	ErrNoState = errors.New("procctl: region has no state block")
	// ErrStateBusy: the state block kept changing during reads.
	ErrStateBusy = errors.New("procctl: state block busy")
	// ErrStateInvalid: the state block failed validation.
	ErrStateInvalid = errors.New("procctl: state block invalid")
	// ErrTimeout: a remote read did not complete in time (SPEC §13).
	ErrTimeout = errors.New("procctl: remote read timed out")
)

// remoteRead copies len(dst) bytes from addr in process pid.
func remoteRead(pid int, addr uint64, dst []byte) error {
	local := []unix.Iovec{{Base: &dst[0]}}
	local[0].SetLen(len(dst))
	remote := []unix.RemoteIovec{{Base: uintptr(addr), Len: len(dst)}}
	n, err := unix.ProcessVMReadv(pid, local, remote, 0)
	if err != nil {
		return err
	}
	if n != len(dst) {
		return unix.EFAULT
	}
	return nil
}

func remoteReadU64(pid int, addr uint64) (uint64, error) {
	var v uint64
	b := unsafe.Slice((*byte)(unsafe.Pointer(&v)), 8)
	if err := remoteRead(pid, addr, b); err != nil {
		return 0, err
	}
	return le.Uint64(b), nil
}

// readStateOnce performs the SPEC §7.3 procedure once.
func readStateOnce(pid int, h *Header, innermostNSPid int) (State, error) {
	var st State
	s1, err := remoteReadU64(pid, h.StateAddr+sSeq)
	if err != nil {
		return st, err
	}
	if s1&1 == 1 || s1 == 0 {
		return st, ErrStateBusy
	}
	buf := make([]byte, h.StateSize)
	if err := remoteRead(pid, h.StateAddr, buf); err != nil {
		return st, err
	}
	s2, err := remoteReadU64(pid, h.StateAddr+sSeq)
	if err != nil {
		return st, err
	}
	if s1 != s2 || le.Uint64(buf[sSeq:]) != s1 {
		return st, ErrStateBusy
	}
	if string(buf[sMagic:sMagic+8]) != stateMagic || le.Uint64(buf[sInstance:]) != h.InstanceID {
		return st, ErrStateInvalid
	}
	slots := int(h.ResourceSlots)
	n := int(le.Uint32(buf[sLen:]))
	used := StateHdrSize + slots*EntrySize + n
	if used > len(buf) {
		return st, ErrStateInvalid
	}
	want := le.Uint32(buf[sCRC:])
	chk := make([]byte, used)
	copy(chk, buf[:used])
	clear(chk[sSeq : sSeq+8])
	clear(chk[sCRC : sCRC+4])
	if crc32c(chk) != want {
		return st, ErrStateBusy // torn across the syscalls, or corrupt
	}
	st.Seq = s1
	st.WriterPID = le.Uint32(buf[sPID:])
	st.Stale = int(st.WriterPID) != innermostNSPid
	st.AppliedGen = le.Uint64(buf[sApplied:])
	st.AppliedWriterID = le.Uint64(buf[sAppliedW:])
	st.GrantsSeenGen = le.Uint64(buf[sSeen:])
	if st.Stale {
		return st, nil
	}
	for i := 0; i < slots; i++ {
		e := buf[StateHdrSize+i*EntrySize:]
		r := ResourceEntry{
			GrantID:        le.Uint64(e[eGrant:]),
			LastActivityNS: le.Uint64(e[eActivity:]),
			Kind:           le.Uint32(e[eKind:]),
			State:          le.Uint32(e[eState:]),
			Flags:          le.Uint32(e[eFlags:]),
			Error:          le.Uint32(e[eError:]),
		}
		copy(r.ID[:], e[eID:eID+16])
		if r.Kind == 0 && r.ID == ([16]byte{}) {
			continue
		}
		st.Resources = append(st.Resources, r)
	}
	st.Consumer = append([]byte(nil), buf[StateHdrSize+slots*EntrySize:used]...)
	return st, nil
}

// readState retries readStateOnce under a deadline. A read that is still
// blocked when ctx expires is abandoned (its goroutine finishes on its own).
func readState(ctx context.Context, pid int, h *Header, innermost int, timeout time.Duration) (State, error) {
	if h.StateAddr == 0 {
		return State{}, ErrNoState
	}
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
			r.st, r.err = readStateOnce(pid, h, innermost)
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
