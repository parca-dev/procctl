CC     ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror
BIN    := bin
SAN    := -fsanitize=address,undefined -fno-sanitize-recover=all

.PHONY: all test test-c test-c-compile test-go demo clean

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

demo: all
	examples/echo/demo.sh

clean:
	rm -rf $(BIN)
