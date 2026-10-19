// Package procctl is the agent side of the procctl control channel
// (SPEC.md 1.0-draft r2): it discovers regions created by in-process
// libraries, writes configuration and exclusive-resource grants into them,
// and reads each process's state block remotely.
package procctl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Spec version implemented by this package.
const (
	SpecMajor = 1
	SpecMinor = 0
)

// Layout sizes and limits (SPEC §4–§6).
const (
	HeaderSize       = 128
	CtrlSize         = 64
	EntrySize        = 48
	StateHdrSize     = 64
	MaxSlots         = 64
	MaxStateSize     = 65536
	DefaultMaxRegion = 4 << 20
	signature        = "PROC_CTL"
	stateMagic       = "PROCSTAT"
	memfdPrefix      = "/memfd:procctl:"
)

// Header field offsets.
const (
	hSig       = 0
	hMajor     = 8
	hMinor     = 10
	hHdrSize   = 12
	hTotal     = 16
	hPage      = 24
	hFlags     = 28
	hConsumer  = 32
	hSchema    = 64
	hCreator   = 68
	hAreaOff   = 72
	hAreaSize  = 80
	hStateAddr = 88
	hStateSize = 96
	hSlots     = 100
	hCreated   = 104
	hInstance  = 112
	hCRC       = 120
)

// Control block offsets (SPEC §5.1).
const (
	cSeq         = 0
	cLen         = 8
	cFlags       = 12
	cWriterID    = 16
	cHeartbeat   = 24
	cLease       = 32
	cWriterPID   = 40
	cWriterFlags = 44
	cWriterStart = 48
	cCRC         = 56
)

// Control block flag bits.
const (
	FlagPayloadValid = 1 << 0
	writerReleased   = 1 << 0
)

// State block offsets (SPEC §6).
const (
	sMagic    = 0
	sSeq      = 8
	sInstance = 16
	sPID      = 24
	sLen      = 28
	sApplied  = 32
	sAppliedW = 40
	sSeen     = 48
	sCRC      = 56
)

// Entry offsets: grant table (§5.2) and resource table (§6.1).
const (
	eID       = 0
	eGrant    = 16
	eDuration = 24
	eActivity = 24
	eKind     = 32
	eState    = 36
	eFlags    = 40
	eError    = 44
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

var le = binary.LittleEndian

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
	if len(b) < HeaderSize {
		return h, invalid("short header")
	}
	if string(b[hSig:hSig+8]) != signature {
		return h, invalid("bad signature")
	}
	if crc32c(b[:hCRC]) != le.Uint32(b[hCRC:]) {
		return h, invalid("header crc mismatch")
	}
	h.SpecMajor = le.Uint16(b[hMajor:])
	h.SpecMinor = le.Uint16(b[hMinor:])
	if h.SpecMajor != SpecMajor {
		return h, invalid("spec major %d not supported", h.SpecMajor)
	}
	h.TotalSize = le.Uint64(b[hTotal:])
	h.PageSize = le.Uint32(b[hPage:])
	h.SchemaVersion = le.Uint32(b[hSchema:])
	h.CreatorPID = le.Uint32(b[hCreator:])
	h.AreaOff = le.Uint64(b[hAreaOff:])
	h.AreaSize = le.Uint64(b[hAreaSize:])
	h.StateAddr = le.Uint64(b[hStateAddr:])
	h.StateSize = le.Uint32(b[hStateSize:])
	h.ResourceSlots = le.Uint16(b[hSlots:])
	h.CreatedAtNS = le.Uint64(b[hCreated:])
	h.InstanceID = le.Uint64(b[hInstance:])

	name := b[hConsumer : hConsumer+32]
	n := 0
	for n < len(name) && name[n] != 0 {
		c := name[n]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return h, invalid("bad consumer name")
		}
		n++
	}
	if n == 0 || n > 31 {
		return h, invalid("bad consumer name length")
	}
	for _, c := range name[n:] {
		if c != 0 {
			return h, invalid("consumer not NUL-padded")
		}
	}
	h.Consumer = string(name[:n])

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
	case CtrlSize+uint64(h.ResourceSlots)*EntrySize > h.AreaSize:
		return h, invalid("grant table does not fit")
	case h.InstanceID == 0:
		return h, invalid("zero instance_id")
	}
	if h.StateAddr != 0 {
		switch {
		case h.StateAddr%8 != 0:
			return h, invalid("state_addr not aligned")
		case h.StateSize%8 != 0 || h.StateSize < StateHdrSize || h.StateSize > MaxStateSize:
			return h, invalid("bad state_size %d", h.StateSize)
		case StateHdrSize+uint32(h.ResourceSlots)*EntrySize > h.StateSize:
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
	b := make([]byte, HeaderSize)
	copy(b[hSig:], signature)
	le.PutUint16(b[hMajor:], h.SpecMajor)
	le.PutUint16(b[hMinor:], h.SpecMinor)
	le.PutUint32(b[hHdrSize:], HeaderSize)
	le.PutUint64(b[hTotal:], h.TotalSize)
	le.PutUint32(b[hPage:], h.PageSize)
	copy(b[hConsumer:hConsumer+31], h.Consumer)
	le.PutUint32(b[hSchema:], h.SchemaVersion)
	le.PutUint32(b[hCreator:], h.CreatorPID)
	le.PutUint64(b[hAreaOff:], h.AreaOff)
	le.PutUint64(b[hAreaSize:], h.AreaSize)
	le.PutUint64(b[hStateAddr:], h.StateAddr)
	le.PutUint32(b[hStateSize:], h.StateSize)
	le.PutUint16(b[hSlots:], h.ResourceSlots)
	le.PutUint64(b[hCreated:], h.CreatedAtNS)
	le.PutUint64(b[hInstance:], h.InstanceID)
	le.PutUint32(b[hCRC:], crc32c(b[:hCRC]))
	return b
}
