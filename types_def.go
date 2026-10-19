//go:build ignore

// Input to `go tool cgo -godefs` (see scripts/gen-types.sh): the wire layout
// is defined once, in c/procctl.h, and the Go types in types_gen.go are
// generated from it.

package procctl

/*
#include "procctl.h"
*/
import "C"

// Wire structs (SPEC §4–§6).
type wireHeader C.struct_procctl_wire_header
type ctrlBlock C.struct_procctl_ctrl
type grantEntry C.struct_procctl_grant_entry
type stateHeader C.struct_procctl_state_hdr
type resEntry C.struct_procctl_res_entry

// Spec version implemented by this package.
const (
	SpecMajor = C.PROCCTL_SPEC_MAJOR
	SpecMinor = C.PROCCTL_SPEC_MINOR
)

// Limits (SPEC §4, §6).
const (
	MaxSlots     = C.PROCCTL_MAX_SLOTS
	MaxStateSize = C.PROCCTL_MAX_STATE_SIZE
)

// Control block flags (SPEC §5.1).
const (
	FlagPayloadValid = C.PROCCTL_CTRL_F_PAYLOAD_VALID
	writerReleased   = C.PROCCTL_WRITER_F_RELEASED
)

// Resource states and flags (SPEC §6.1).
const (
	ResIdle      = C.PROCCTL_RES_IDLE
	ResAcquiring = C.PROCCTL_RES_ACQUIRING
	ResHolding   = C.PROCCTL_RES_HOLDING
	ResReleased  = C.PROCCTL_RES_RELEASED
	ResFailed    = C.PROCCTL_RES_FAILED
	ResFlagWant  = C.PROCCTL_RES_F_WANT
)
