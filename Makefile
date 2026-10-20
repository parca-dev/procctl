CC     ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror
BIN    := bin
SAN    := -fsanitize=address,undefined -fno-sanitize-recover=all

# Regenerate types_gen.go from the wire structs in c/procctl.h (go tool cgo -godefs).
generate:
	scripts/gen-types.sh

.PHONY: images e2e-kind generate all test test-c test-c-compile test-go test-hello bench fuzz-go fuzz-c model-long kernel-report demo clean

all: $(BIN)/echo-client $(BIN)/echo-agent

$(BIN):
	mkdir -p $(BIN)

$(BIN)/echo-client: examples/echo/client.c c/procctl.h | $(BIN)
	$(CC) -std=gnu11 -O2 -Wall -Wextra -Werror -Ic -o $@ $<

$(BIN)/echo-agent: $(shell find . -name '*.go' -not -path './spike/*') go.mod | $(BIN)
	go build -o $@ ./examples/echo/agent

test: test-c-compile test-c test-go

# procctl.h must build cleanly as C and C++, with the system and bundled sdt.h.
test-c-compile:
	@for v in "" "-DPROCCTL_BUNDLED_SDT"; do \
	  for c in "gcc -std=gnu11" "clang -std=gnu11" "gcc -std=c11 -D_GNU_SOURCE" "clang -std=c11 -D_GNU_SOURCE"; do \
	    $$c $$v -Wall -Wextra -Werror -pedantic -c c/tests/compile_test.c -o /dev/null || exit 1; done; \
	  for c in "g++ -std=c++17" "clang++ -std=c++17"; do \
	    $$c $$v -Wall -Wextra -Werror -pedantic -c c/tests/compile_test.cc -o /dev/null || exit 1; done; \
	done; echo "procctl.h: 12 compile configurations clean"

test-c: | $(BIN)
	$(CC) -std=c11 $(CFLAGS) $(SAN) -o $(BIN)/unit_test c/tests/unit_test.c -lpthread
	$(BIN)/unit_test

test-go:
	go vet ./...
	go test -race -count=1 ./...

# The hello watcher needs root (BPF, uprobes, perf). Runs in the initial pid
# namespace and nested in a new one (the kind case: pids are translated).
test-hello:
	go test -c -o $(BIN)/hello.test ./hello
	cd hello && sudo $(abspath $(BIN))/hello.test -test.v
	cd hello && sudo unshare --pid --fork --mount-proc $(abspath $(BIN))/hello.test -test.v

# poll() microbenchmarks. PROCCTL_BENCH_ENFORCE=1 fails if the no-change path is >= 2 ns.
bench: | $(BIN)
	$(CC) -std=gnu11 -O2 -Ic -o $(BIN)/bench_poll c/tests/bench_poll.c
	$(BIN)/bench_poll

FUZZTIME ?= 30s
fuzz-go:
	@for f in FuzzParseHeader FuzzParseState FuzzAreaRead; do \
	  go test -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZTIME) . || exit 1; done

FUZZSECS ?= 30
fuzz-c: | $(BIN)
	clang -std=gnu11 -g -O1 -fsanitize=fuzzer,address,undefined -o $(BIN)/fuzz_area c/tests/fuzz_area.c
	$(BIN)/fuzz_area -max_total_time=$(FUZZSECS) -print_final_stats=1

# Arbiter model tests: 5000 random schedules x 10000 steps.
model-long:
	go test -count=1 -timeout 2h -run TestModel ./exclusive -args -long

kernel-report:
	@uname -a; echo "page size: $$(getconf PAGESIZE)"; \
	echo "yama ptrace_scope: $$(cat /proc/sys/kernel/yama/ptrace_scope 2>/dev/null || echo n/a)"; \
	echo "perf_event_paranoid: $$(cat /proc/sys/kernel/perf_event_paranoid)"; \
	echo "unprivileged_userfaultfd: $$(cat /proc/sys/vm/unprivileged_userfaultfd 2>/dev/null || echo n/a)"

# Test images for the e2e suite (local, for kind).
images:
	docker build -f testclient/Dockerfile -t procctl-testclient:dev .
	docker build -f cmd/procctl-testagent/Dockerfile -t procctl-testagent:dev .

# The e2e suite on a kind cluster named procctl-e2e (created if needed;
# E2E_KEEP_CLUSTER=1 keeps it afterwards).
e2e-kind: images
	cd e2e && go test -count=1 -timeout 45m -v ./...

demo: all
	examples/echo/demo.sh

clean:
	rm -rf $(BIN)
