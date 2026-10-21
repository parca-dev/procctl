//go:build linux

package procctl

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// State is a consistent copy of a process's state block.
type State struct {
	Seq        uint64
	AppliedGen uint64 // latest generation the process acknowledged
	PID        uint32 // pid of the writer, in the process's pid namespace
	// Stale: written by another process (a forked child's inherited copy).
	// A stale block reports nothing.
	Stale    bool
	Consumer []byte // the consumer state
}

var (
	// ErrStateBusy: the state block kept changing while being read.
	ErrStateBusy = errors.New("procctl: state block busy")
	// ErrStateInvalid: the state block failed validation.
	ErrStateInvalid = errors.New("procctl: state block invalid")
	// ErrTimeout: the remote read did not finish in time.
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

// readStateOnce is one seqlock read: seq, the block, seq again.
func readStateOnce(pid int, h *Header, innermostPID int) (State, error) {
	var seq [8]byte
	if err := remoteRead(pid, h.StateAddr+uint64(stateSeqOff), seq[:]); err != nil {
		return State{}, err
	}
	s1 := le.Uint64(seq[:])
	if s1&1 == 1 || s1 == 0 {
		return State{}, ErrStateBusy
	}
	buf := make([]byte, h.StateSize)
	if err := remoteRead(pid, h.StateAddr, buf); err != nil {
		return State{}, err
	}
	if err := remoteRead(pid, h.StateAddr+uint64(stateSeqOff), seq[:]); err != nil {
		return State{}, err
	}
	if le.Uint64(seq[:]) != s1 || le.Uint64(buf[stateSeqOff:]) != s1 {
		return State{}, ErrStateBusy
	}
	return parseState(buf, h, innermostPID)
}

// parseState validates and decodes a state block copied out of a process.
func parseState(buf []byte, h *Header, innermostPID int) (State, error) {
	var sb stateBlock
	if len(buf) < StateSize || decode(buf, &sb) != nil {
		return State{}, ErrStateInvalid
	}
	if string(sb.Magic[:]) != stateMagic || sb.Instance_id != h.InstanceID ||
		StateSize+int(sb.Len) > len(buf) {
		return State{}, ErrStateInvalid
	}
	st := State{Seq: sb.Seq, PID: sb.Pid, Stale: int(sb.Pid) != innermostPID}
	if !st.Stale {
		st.AppliedGen = sb.Applied_gen
		st.Consumer = append([]byte(nil), buf[StateSize:StateSize+int(sb.Len)]...)
	}
	return st, nil
}

// readState retries readStateOnce under a time bound: a process can make its
// own memory slow to read, and must not hold up the agent.
func readState(ctx context.Context, pid int, h *Header, innermostPID int, timeout time.Duration) (State, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		st  State
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var r result
		for tries := 0; tries < 8; tries++ {
			if r.st, r.err = readStateOnce(pid, h, innermostPID); !errors.Is(r.err, ErrStateBusy) {
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
