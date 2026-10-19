#!/usr/bin/env bash
# Generates Go types from C wire structs with `go tool cgo -godefs` (the
# approach ebpf-profiler uses in support/):
#
#   types_gen.go                      from c/procctl.h          (procctl wire format)
#   cmd/procctl-testagent/types_gen.go from testclient/testclient.h (a consumer schema)
#
#   scripts/gen-types.sh          regenerate
#   scripts/gen-types.sh --check  fail if any generated file is stale
#
# Needs a C compiler; users of the Go module don't (the output is committed).
set -euo pipefail
cd "$(dirname "$0")/.."
root=$PWD

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"; find "$root" -name _obj -type d -prune -exec rm -rf {} +' EXIT

stale=0
gen() { # dir include-dirs...
	local dir=$1; shift
	local flags=()
	for inc in "$@"; do flags+=("-I$(realpath --relative-to="$root/$dir" "$root/$inc")"); done
	local out="$tmp/$(echo "$dir" | tr / _).go"
	{
		echo "// SPDX-License-Identifier: Apache-2.0"
		echo
		(cd "$root/$dir" && go tool cgo -godefs -- "${flags[@]}" types_def.go)
	} > "$out"
	gofmt -w "$out"
	if [[ "$check" == 1 ]]; then
		if ! diff -u "$root/$dir/types_gen.go" "$out"; then
			echo "$dir/types_gen.go is stale: run scripts/gen-types.sh" >&2
			stale=1
		fi
	else
		cp "$out" "$root/$dir/types_gen.go"
	fi
}

check=0
[[ "${1:-}" == "--check" ]] && check=1
gen . c
gen cmd/procctl-testagent testclient
if [[ "$check" == 1 ]]; then
	[[ "$stale" == 0 ]] && echo "generated types are up to date"
	exit "$stale"
fi
