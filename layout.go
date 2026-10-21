// Package procctl is the agent side of procctl: it finds the regions that
// in-process libraries create, writes configuration into them, and reads each
// process's answers from its state block (SPEC.md).
package procctl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"unsafe"
)

//go:generate scripts/gen-types.sh

// The wire layout is defined once, in c/procctl.h; types_gen.go is generated
// from it with cgo -godefs.

const (
	HeaderSize = int(unsafe.Sizeof(wireHeader{}))
	CtrlSize   = int(unsafe.Sizeof(ctrlBlock{}))
	StateSize  = int(unsafe.Sizeof(stateBlock{})) // the state block's header; consumer state follows

	stateSeqOff = int(unsafe.Offsetof(stateBlock{}.Seq))

	// DefaultMaxRegion is the largest region an agent maps by default.
	DefaultMaxRegion = 4 << 20

	magic       = "PROC_CTL"
	stateMagic  = "PROCSTAT"
	memfdPrefix = "/memfd:procctl:"
)

var (
	le         = binary.LittleEndian
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

func decode[T any](b []byte, v *T) error { return binary.Read(bytes.NewReader(b), le, v) }

// Header is a validated copy of a region header.
type Header struct {
	Version       uint16
	PageSize      uint32 // the agent area starts here
	TotalSize     uint64
	Consumer      string
	SchemaVersion uint32
	StateAddr     uint64 // in the process's address space
	StateSize     uint32
	InstanceID    uint64
}

// PayloadCapacity is the largest payload the region can carry.
func (h Header) PayloadCapacity() int { return int(h.TotalSize) - int(h.PageSize) - CtrlSize }

// ErrInvalid wraps every validation failure of a region.
var ErrInvalid = errors.New("procctl: invalid region")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// ParseHeader validates a header copied out of a region of fileSize bytes.
// Everything in it comes from an untrusted process.
func ParseHeader(b []byte, fileSize uint64) (Header, error) {
	var w wireHeader
	if len(b) < HeaderSize || decode(b, &w) != nil {
		return Header{}, invalid("short header")
	}
	if string(w.Magic[:]) != magic {
		return Header{}, invalid("bad magic")
	}
	if w.Version != Version {
		return Header{}, invalid("version %d not supported", w.Version)
	}
	consumer, ok := parseConsumer(w.Consumer[:])
	if !ok {
		return Header{}, invalid("bad consumer name")
	}
	h := Header{
		Version: w.Version, PageSize: w.Page_size, TotalSize: w.Total_size, Consumer: consumer,
		SchemaVersion: w.Schema_version, StateAddr: w.State_addr, StateSize: w.State_size,
		InstanceID: w.Instance_id,
	}
	p := uint64(h.PageSize)
	switch {
	case int(w.Header_size) < HeaderSize:
		return h, invalid("header_size %d", w.Header_size)
	case p < 4096 || p > 65536 || p&(p-1) != 0:
		return h, invalid("page_size %d", p)
	case h.TotalSize != fileSize:
		return h, invalid("total_size %d != file size %d", h.TotalSize, fileSize)
	case h.TotalSize < p+uint64(CtrlSize):
		return h, invalid("total_size %d too small", h.TotalSize)
	case h.InstanceID == 0:
		return h, invalid("instance_id 0")
	case h.StateAddr == 0 || h.StateAddr%8 != 0:
		return h, invalid("state_addr %#x", h.StateAddr)
	case h.StateSize%8 != 0 || h.StateSize < uint32(StateSize) || h.StateSize > MaxStateSize:
		return h, invalid("state_size %d", h.StateSize)
	}
	return h, nil
}

// parseConsumer checks a NUL-padded [a-z0-9_]{1,31}.
func parseConsumer(b []byte) (string, bool) {
	n := bytes.IndexByte(b, 0)
	if n <= 0 || n > 31 {
		return "", false
	}
	for _, c := range b[:n] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return "", false
		}
	}
	for _, c := range b[n:] {
		if c != 0 {
			return "", false
		}
	}
	return string(b[:n]), true
}

// encodeHeader builds a header (for tests).
func encodeHeader(h Header) []byte {
	w := wireHeader{
		Version: h.Version, Header_size: uint16(HeaderSize), Page_size: h.PageSize,
		Total_size: h.TotalSize, Schema_version: h.SchemaVersion, State_size: h.StateSize,
		State_addr: h.StateAddr, Instance_id: h.InstanceID,
	}
	copy(w.Magic[:], magic)
	copy(w.Consumer[:31], h.Consumer)
	var buf bytes.Buffer
	_ = binary.Write(&buf, le, &w)
	return buf.Bytes()
}
