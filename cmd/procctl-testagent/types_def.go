//go:build ignore

// Input to `go tool cgo -godefs` (scripts/gen-types.sh): the test client's
// wire structs are defined once, in testclient/testclient.h.

package main

/*
#include "testclient.h"
*/
import "C"

type payloadHeader C.struct_testclient_payload
type clientStatus C.struct_testclient_status

const (
	consumerName  = C.TESTCLIENT_CONSUMER
	schemaVersion = C.TESTCLIENT_SCHEMA_VERSION
	kindToken     = C.TESTCLIENT_KIND_TOKEN
	modeExclusive = C.TESTCLIENT_MODE_EXCLUSIVE
)
