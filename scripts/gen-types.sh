#!/usr/bin/env bash
# Generates types_gen.go from the wire structs in c/procctl.h with
# `go tool cgo -godefs` (the approach ebpf-profiler uses in support/).
#
#   scripts/gen-types.sh          regenerate types_gen.go
#   scripts/gen-types.sh --check  fail if types_gen.go is stale
#
# Needs a C compiler; users of the Go module don't (the output is committed).
set -euo pipefail
cd "$(dirname "$0")/.."

tmp=$(mktemp -d)
trap 'rm -rf "$tmp" _obj' EXIT

{
	echo "// SPDX-License-Identifier: Apache-2.0"
	echo
	go tool cgo -godefs -- -Ic types_def.go
} > "$tmp/types_gen.go"
gofmt -w "$tmp/types_gen.go"

if [[ "${1:-}" == "--check" ]]; then
	if ! diff -u types_gen.go "$tmp/types_gen.go"; then
		echo "types_gen.go is stale: run scripts/gen-types.sh" >&2
		exit 1
	fi
	echo "types_gen.go is up to date"
else
	cp "$tmp/types_gen.go" types_gen.go
fi
