//go:build ignore

// Input to `go tool cgo -godefs` (scripts/gen-types.sh): the wire layout is
// defined once, in c/procctl.h, and types_gen.go is generated from it.

package procctl

/*
#include "procctl.h"
*/
import "C"

type wireHeader C.struct_procctl_header
type ctrlBlock C.struct_procctl_ctrl
type stateBlock C.struct_procctl_state

const (
	// Version is the wire format version, also part of the memfd name.
	Version      = C.PROCCTL_VERSION
	MaxStateSize = C.PROCCTL_MAX_STATE_SIZE
	flagReleased = C.PROCCTL_RELEASED
)
