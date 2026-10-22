CC     ?= cc
BIN    := bin
SAN    := -fsanitize=address,undefined -fno-sanitize-recover=all

.PHONY: all test test-c test-go bench fuzz generate images images-push e2e-kind clean

all: $(BIN)/echo-client $(BIN)/echo-agent

$(BIN):
	mkdir -p $(BIN)

$(BIN)/echo-client: examples/echo/client.c c/procctl.h | $(BIN)
	$(CC) -std=gnu11 -O2 -Wall -Wextra -Werror -Ic -o $@ $<

$(BIN)/echo-agent: $(wildcard *.go examples/echo/agent/*.go) go.mod | $(BIN)
	go build -o $@ ./examples/echo/agent

test: test-c test-go

# procctl.h builds cleanly as C and C++ with gcc and clang; its unit tests run
# under ASan and UBSan.
test-c: | $(BIN)
	for c in "gcc -std=gnu11" "clang -std=gnu11"; do \
	  $$c -Wall -Wextra -Werror -pedantic -c c/tests/compile_test.c -o /dev/null || exit 1; done
	for c in "g++ -std=c++17" "clang++ -std=c++17"; do \
	  $$c -Wall -Wextra -Werror -pedantic -c c/tests/compile_test.cc -o /dev/null || exit 1; done
	$(CC) -std=c11 -O1 -g -Wall -Wextra -Werror $(SAN) -o $(BIN)/unit_test c/tests/unit_test.c
	$(BIN)/unit_test

test-go:
	go vet ./...
	go test -race -count=1 ./...

# poll() microbenchmark.
bench: | $(BIN)
	$(CC) -std=gnu11 -O2 -Ic -o $(BIN)/bench_poll c/tests/bench_poll.c
	$(BIN)/bench_poll

FUZZTIME ?= 30s
fuzz:
	go test -run '^$$' -fuzz '^FuzzParseHeader$$' -fuzztime $(FUZZTIME) .
	go test -run '^$$' -fuzz '^FuzzParseState$$' -fuzztime $(FUZZTIME) .

# Regenerates the Go wire types from the C headers (cgo -godefs).
generate:
	scripts/gen-types.sh

# Test images for the e2e suite.
images:
	docker build -f testclient/Dockerfile -t procctl-testclient:dev .
	docker build -f cmd/procctl-testagent/Dockerfile -t procctl-testagent:dev .

# Push the test images for another cluster's nodes:
#   make images-push REGISTRY=ghcr.io/parca-dev [PLATFORM=linux/arm64]
# (the default builder: it uses the host's network and DNS)
TAG      ?= $(shell git rev-parse --short=12 HEAD)
PLATFORM ?= linux/amd64
images-push:
	@test -n "$(REGISTRY)" || { echo "set REGISTRY"; exit 2; }
	docker buildx build --builder default --platform $(PLATFORM) -f testclient/Dockerfile -t $(REGISTRY)/procctl-testclient:$(TAG) --push .
	docker buildx build --builder default --platform $(PLATFORM) -f cmd/procctl-testagent/Dockerfile -t $(REGISTRY)/procctl-testagent:$(TAG) --push .

# The e2e suite on a kind cluster named procctl-e2e (created if needed;
# E2E_KEEP_CLUSTER=1 keeps it, E2E_KIND_IMAGE=kindest/node:vX picks the
# Kubernetes version). Any other one-node cluster: go test ./e2e -args
# -kubeconfig ... -image-registry ... -image-tag ...
e2e-kind: images
	cd e2e && go test -count=1 -timeout 20m -parallel 4 -v ./...

clean:
	rm -rf $(BIN)
