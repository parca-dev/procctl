// Package procctl is the agent side of the procctl control channel
// (SPEC.md 1.0-draft r2): it discovers regions created by in-process
// libraries, writes configuration and exclusive-resource grants into them,
// and reads each process's state block remotely.
package procctl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sync/atomic"
	"unsafe"
)

// Spec version implemented by this package.
const (
	SpecMajor = 1
	SpecMinor = 0
)

// Limits (SPEC §4, §10.2).
const (
	MaxSlots         = 64
	MaxStateSize     = 65536
	DefaultMaxRegion = 4 << 20
)

// ---- Wire structs -------------------------------------------------------------
//
// These mirror SPEC §4–§6 field for field. All layout sizes and offsets are
// derived from them; the assertions below pin each one to the size the spec
// gives, and TestLayoutMatchesC checks every field offset against procctl.h.
// Encoding is little-endian; the agent runs only on little-endian hosts.

// wireHeader is the region header (SPEC §4).
type wireHeader struct {
	Signature     [8]byte
	SpecMajor     uint16
	SpecMinor     uint16
	HeaderSize    uint32
	TotalSize     uint64
	PageSize      uint32
	Flags         uint32
	Consumer      [32]byte
	SchemaVersion uint32
	CreatorPID    uint32
	AgentAreaOff  uint64
	AgentAreaSize uint64
	StateAddr     uint64
	StateSize     uint32
	ResourceSlots uint16
	_             uint16
	CreatedAtNS   uint64
	InstanceID    uint64
	HeaderCRC     uint32
	_             uint32
}

// ctrlBlock is the agent area's control block (SPEC §5.1). The agent
// accesses it in place, through atomics, overlaid on the shared mapping.
type ctrlBlock struct {
	Seq          atomic.Uint64
	PayloadLen   atomic.Uint32
	Flags        atomic.Uint32
	WriterID     atomic.Uint64
	HeartbeatNS  atomic.Uint64
	LeaseNS      atomic.Uint64
	WriterPID    atomic.Uint32
	WriterFlags  atomic.Uint32
	WriterStart  atomic.Uint64
	PayloadCRC32 atomic.Uint32
	_            uint32
}

// Grant is one grant-table entry (SPEC §5.2).
type Grant struct {
	ID       [16]byte
	GrantID  uint64 // 0 on Write means "new grant": it gets the write's generation (§9.3)
	Duration uint64 // nanoseconds
	Kind     uint32
	_        uint32 // flags, reserved
	_        uint64
}

// stateHeader is the fixed part of a state block (SPEC §6).
type stateHeader struct {
	Magic           [8]byte
	Seq             uint64
	InstanceID      uint64
	WriterPID       uint32
	StateLen        uint32
	AppliedGen      uint64
	AppliedWriterID uint64
	GrantsSeenGen   uint64
	CRC32           uint32
	_               uint32
}

// ResourceEntry is one resource-table entry of a state block (SPEC §6.1).
type ResourceEntry struct {
	ID             [16]byte
	GrantID        uint64
	LastActivityNS uint64
	Kind           uint32
	State          uint32
	Flags          uint32
	Error          uint32
}

// Sizes, derived from the structs.
const (
	HeaderSize   = int(unsafe.Sizeof(wireHeader{}))
	CtrlSize     = int(unsafe.Sizeof(ctrlBlock{}))
	EntrySize    = int(unsafe.Sizeof(Grant{}))
	StateHdrSize = int(unsafe.Sizeof(stateHeader{}))
	resEntrySize = int(unsafe.Sizeof(ResourceEntry{}))
	headerCRCOff = int(unsafe.Offsetof(wireHeader{}.HeaderCRC))
	stateSeqOff  = int(unsafe.Offsetof(stateHeader{}.Seq))
	stateCRCOff  = int(unsafe.Offsetof(stateHeader{}.CRC32))
)

// Compile-time checks that the structs have exactly the spec's sizes (a
// negative array length fails the build) and that both entry kinds agree.
var (
	_ [HeaderSize - 128]struct{}
	_ [128 - HeaderSize]struct{}
	_ [CtrlSize - 64]struct{}
	_ [64 - CtrlSize]struct{}
	_ [EntrySize - 48]struct{}
	_ [48 - EntrySize]struct{}
	_ [resEntrySize - EntrySize]struct{}
	_ [EntrySize - resEntrySize]struct{}
	_ [StateHdrSize - 64]struct{}
	_ [64 - StateHdrSize]struct{}
)

const (
	signature   = "PROC_CTL"
	stateMagic  = "PROCSTAT"
	memfdPrefix = "/memfd:procctl:"

	// Control block flags (SPEC §5.1).
	FlagPayloadValid = 1 << 0
	writerReleased   = 1 << 0
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

var le = binary.LittleEndian

// decode reads a fixed-size wire struct from the front of b.
func decode[T any](b []byte, v *T) error {
	return binary.Read(bytes.NewReader(b), le, v)
}

// encode appends a wire struct (or slice of them) to dst.
func encode(dst *bytes.Buffer, v any) {
	if err := binary.Write(dst, le, v); err != nil {
		panic(err) // only fixed-size wire structs are encoded
	}
}

func isLittleEndian() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}

// Header is a validated snapshot of a region header. Agents use only the
// snapshot after attach (SPEC §10.2 step 7).
type Header struct {
	SpecMajor, SpecMinor uint16
	TotalSize            uint64
	PageSize             uint32
	Consumer             string
	SchemaVersion        uint32
	CreatorPID           uint32
	AreaOff, AreaSize    uint64
	StateAddr            uint64
	StateSize            uint32
	ResourceSlots        uint16
	CreatedAtNS          uint64
	InstanceID           uint64
}

// ErrInvalid wraps every header or layout validation failure.
var ErrInvalid = errors.New("procctl: invalid region")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// ParseHeader validates a header copied out of a region of size fileSize.
func ParseHeader(b []byte, fileSize uint64) (Header, error) {
	var h Header
	var w wireHeader
	if len(b) < HeaderSize || decode(b, &w) != nil {
		return h, invalid("short header")
	}
	if string(w.Signature[:]) != signature {
		return h, invalid("bad signature")
	}
	if crc32c(b[:headerCRCOff]) != w.HeaderCRC {
		return h, invalid("header crc mismatch")
	}
	if w.SpecMajor != SpecMajor {
		return h, invalid("spec major %d not supported", w.SpecMajor)
	}
	n := bytes.IndexByte(w.Consumer[:], 0)
	if n < 0 {
		n = len(w.Consumer)
	}
	for _, c := range w.Consumer[:n] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return h, invalid("bad consumer name")
		}
	}
	if n == 0 || n > 31 {
		return h, invalid("bad consumer name length")
	}
	for _, c := range w.Consumer[n:] {
		if c != 0 {
			return h, invalid("consumer not NUL-padded")
		}
	}
	h = Header{
		SpecMajor: w.SpecMajor, SpecMinor: w.SpecMinor, TotalSize: w.TotalSize,
		PageSize: w.PageSize, Consumer: string(w.Consumer[:n]), SchemaVersion: w.SchemaVersion,
		CreatorPID: w.CreatorPID, AreaOff: w.AgentAreaOff, AreaSize: w.AgentAreaSize,
		StateAddr: w.StateAddr, StateSize: w.StateSize, ResourceSlots: w.ResourceSlots,
		CreatedAtNS: w.CreatedAtNS, InstanceID: w.InstanceID,
	}

	p := uint64(h.PageSize)
	switch {
	case h.TotalSize != fileSize:
		return h, invalid("total_size %d != file size %d", h.TotalSize, fileSize)
	case p < 4096 || p > 65536 || p&(p-1) != 0:
		return h, invalid("bad page size %d", p)
	case h.AreaOff != p:
		return h, invalid("agent_area_off %d != page size", h.AreaOff)
	case h.AreaSize < p || h.AreaSize%p != 0:
		return h, invalid("bad agent_area_size %d", h.AreaSize)
	case h.AreaOff+h.AreaSize != h.TotalSize:
		return h, invalid("areas do not cover the region")
	case h.ResourceSlots > MaxSlots:
		return h, invalid("too many resource slots %d", h.ResourceSlots)
	case uint64(CtrlSize)+uint64(h.ResourceSlots)*uint64(EntrySize) > h.AreaSize:
		return h, invalid("grant table does not fit")
	case h.InstanceID == 0:
		return h, invalid("zero instance_id")
	}
	if h.StateAddr != 0 {
		switch {
		case h.StateAddr%8 != 0:
			return h, invalid("state_addr not aligned")
		case h.StateSize%8 != 0 || h.StateSize < uint32(StateHdrSize) || h.StateSize > MaxStateSize:
			return h, invalid("bad state_size %d", h.StateSize)
		case uint32(StateHdrSize)+uint32(h.ResourceSlots)*uint32(resEntrySize) > h.StateSize:
			return h, invalid("resource table does not fit the state block")
		}
	} else if h.StateSize != 0 {
		return h, invalid("state_size without state_addr")
	}
	return h, nil
}

// PayloadCapacity is the largest consumer payload the region can carry.
func (h *Header) PayloadCapacity() int {
	return int(h.AreaSize) - CtrlSize - int(h.ResourceSlots)*EntrySize
}

// encodeHeader builds a header; used by tests and tools that create regions.
func encodeHeader(h Header) []byte {
	w := wireHeader{
		SpecMajor: h.SpecMajor, SpecMinor: h.SpecMinor, HeaderSize: uint32(HeaderSize),
		TotalSize: h.TotalSize, PageSize: h.PageSize, SchemaVersion: h.SchemaVersion,
		CreatorPID: h.CreatorPID, AgentAreaOff: h.AreaOff, AgentAreaSize: h.AreaSize,
		StateAddr: h.StateAddr, StateSize: h.StateSize, ResourceSlots: h.ResourceSlots,
		CreatedAtNS: h.CreatedAtNS, InstanceID: h.InstanceID,
	}
	copy(w.Signature[:], signature)
	copy(w.Consumer[:31], h.Consumer)
	var buf bytes.Buffer
	encode(&buf, &w)
	b := buf.Bytes()
	le.PutUint32(b[headerCRCOff:], crc32c(b[:headerCRCOff]))
	return b
}
