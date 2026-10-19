#!/usr/bin/env bash
[ "${DEMO_PGRP:-}" = 1 ] || DEMO_PGRP=1 exec setsid "$0" "$@"
# M1 demo on one host (no root needed when Yama ptrace_scope is 0 or the
# agent can ptrace the client):
#   1. the client starts first; the agent discovers it and pushes config
#   2. the client forks: the child shares the region, its state is stale
#   3. kill -9 the agent: the client falls back to defaults after the lease
#   4. restart the agent: config is restored
set -u
cd "$(dirname "$0")/../.."
BIN=bin
LOG=$(mktemp -d)
# Run everything in our own process group so forked children are cleaned up too.
trap 'rm -rf "$LOG"; kill -- -$$ 2>/dev/null' EXIT
ts() { date +%H:%M:%S.%3N; }
step() { echo; echo "[$(ts)] ===== $* ====="; }

step "1. start echo-client (before any agent)"
$BIN/echo-client --interval-ms 50 --fork-after-ms 5000 --exit-after-ms 16000 > "$LOG/client" 2>&1 &
sleep 1

step "start echo-agent (lease 2s, push every 1s)"
$BIN/echo-agent -interval 1s -lease 2s -rescan 500ms 2> "$LOG/agent1" &
AGENT=$!
sleep 4.5

step "2. client forks at 5s (child shares the region; its state block is stale)"
sleep 1.5

step "3. kill -9 the agent"
kill -9 $AGENT
sleep 3.5

step "4. restart the agent"
$BIN/echo-agent -interval 1s -lease 2s -rescan 500ms 2> "$LOG/agent2" &
sleep 3
kill -INT $! 2>/dev/null
sleep 0.5

step "client output"
cat "$LOG/client"
step "agent #1 output (killed with -9)"
sed 's/^time=[^ ]* //' "$LOG/agent1"
step "agent #2 output"
sed 's/^time=[^ ]* //' "$LOG/agent2"
