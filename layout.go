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
	"unsafe"
)

//go:generate scripts/gen-types.sh

// The wire layout is defined once, in c/procctl.h. types_gen.go is generated
// from it by `go tool cgo -godefs` (scripts/gen-types.sh): the wire structs
// (wireHeader, ctrlBlock, grantEntry, stateHeader, resEntry), the spec
// version, limits, flags and resource states. Sizes and the few offsets the
// code needs are derived from those structs.

// DefaultMaxRegion is the default largest region an agent maps (SPEC §10.2).
const DefaultMaxRegion = 4 << 20

// Sizes and offsets, derived from the generated wire structs.
const (
	HeaderSize   = int(unsafe.Sizeof(wireHeader{}))
	CtrlSize     = int(unsafe.Sizeof(ctrlBlock{}))
	EntrySize    = int(unsafe.Sizeof(grantEntry{}))
	StateHdrSize = int(unsafe.Sizeof(stateHeader{}))
	resEntrySize = int(unsafe.Sizeof(resEntry{}))
	headerCRCOff = int(unsafe.Offsetof(wireHeader{}.Header_crc32c))
	stateSeqOff  = int(unsafe.Offsetof(stateHeader{}.Seq))
	stateCRCOff  = int(unsafe.Offsetof(stateHeader{}.Crc32c))
)

const (
	signature   = "PROC_CTL"
	stateMagic  = "PROCSTAT"
	memfdPrefix = "/memfd:procctl:"
)

// Grant is one grant-table entry (SPEC §5.2), as used by the API.
type Grant struct {
	Kind     uint32
	ID       [16]byte
	GrantID  uint64 // 0 on Write means "new grant": it gets the write's generation (§9.3)
	Duration uint64 // nanoseconds
}

func (g Grant) wire() grantEntry {
	return grantEntry{Resource_id: g.ID, Grant_id: g.GrantID, Duration_ns: g.Duration, Kind: g.Kind}
}

func grantFromWire(e grantEntry) Grant {
	return Grant{Kind: e.Kind, ID: e.Resource_id, GrantID: e.Grant_id, Duration: e.Duration_ns}
}

// ResourceEntry is one resource-table entry of a state block (SPEC §6.1), as
// used by the API.
type ResourceEntry struct {
	Kind           uint32
	ID             [16]byte
	GrantID        uint64
	State          uint32
	Flags          uint32
	Error          uint32
	LastActivityNS uint64
}

func resourceFromWire(e resEntry) ResourceEntry {
	return ResourceEntry{
		Kind: e.Kind, ID: e.Resource_id, GrantID: e.Grant_id, State: e.State,
		Flags: e.Flags, Error: e.Error, LastActivityNS: e.Last_activity_ns,
	}
}

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
	if crc32c(b[:headerCRCOff]) != w.Header_crc32c {
		return h, invalid("header crc mismatch")
	}
	if w.Spec_major != SpecMajor {
		return h, invalid("spec major %d not supported", w.Spec_major)
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
		SpecMajor: w.Spec_major, SpecMinor: w.Spec_minor, TotalSize: w.Total_size,
		PageSize: w.Page_size, Consumer: string(w.Consumer[:n]), SchemaVersion: w.Schema_version,
		CreatorPID: w.Creator_pid, AreaOff: w.Agent_area_off, AreaSize: w.Agent_area_size,
		StateAddr: w.State_addr, StateSize: w.State_size, ResourceSlots: w.Resource_slots,
		CreatedAtNS: w.Created_at_ns, InstanceID: w.Instance_id,
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
		Spec_major: h.SpecMajor, Spec_minor: h.SpecMinor, Header_size: uint32(HeaderSize),
		Total_size: h.TotalSize, Page_size: h.PageSize, Schema_version: h.SchemaVersion,
		Creator_pid: h.CreatorPID, Agent_area_off: h.AreaOff, Agent_area_size: h.AreaSize,
		State_addr: h.StateAddr, State_size: h.StateSize, Resource_slots: h.ResourceSlots,
		Created_at_ns: h.CreatedAtNS, Instance_id: h.InstanceID,
	}
	copy(w.Signature[:], signature)
	copy(w.Consumer[:31], h.Consumer)
	var buf bytes.Buffer
	encode(&buf, &w)
	b := buf.Bytes()
	le.PutUint32(b[headerCRCOff:], crc32c(b[:headerCRCOff]))
	return b
}
