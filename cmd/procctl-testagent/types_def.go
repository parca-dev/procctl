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

const consumerName = C.TESTCLIENT_CONSUMER
