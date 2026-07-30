# gdb-wui
#
# Everything here is a thin wrapper over the go tool. The point is not
# abstraction, it is that `make test` runs the same checks CI does, so a green
# local run means something.

GO      ?= go
BIN     ?= gdb-wui
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

FIXTURES := $(patsubst testdata/fixtures/%.c,build/%,$(wildcard testdata/fixtures/*.c))

.PHONY: all build test test-integration test-fuzz lint fmt vet fixtures run vendor-verify clean

all: build

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/gdb-wui

# The default check. -race everywhere: this is a concurrent program whose bugs
# are interleavings, and a non-race run would miss the ones that matter.
test: fmt-check vet
	$(GO) test -race ./...

# Needs gdb and gcc. Skips rather than fails when they are missing, so a
# contributor without them still gets a useful `make test`.
test-integration: fmt-check vet
	$(GO) test -tags integration -race -timeout 10m ./...

# Fuzzing is a nightly job, not a pre-commit one; this is the manual entry point.
test-fuzz:
	$(GO) test ./internal/mi/ -run FuzzParseRecord -fuzz FuzzParseRecord -fuzztime 60s

vet:
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

fmt:
	gofmt -w .

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

# golangci-lint is optional: it is not in every distribution's packages, and
# `go vet` plus the tests carry the load.
lint:
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run \
		|| echo "golangci-lint not installed; skipping"

vendor-verify:
	$(GO) test ./internal/assets/ -run 'TestVendored'

# Compile the fixtures into build/ for poking at by hand. The tests compile
# their own into temp dirs, so this is only for interactive use.
fixtures: $(FIXTURES)

build/%: testdata/fixtures/%.c
	@mkdir -p build
	gcc -g -O0 -pthread -o $@ $<

# The stripped fixture has to be built without -g, which the pattern rule above
# cannot express.
build/nodebug: testdata/fixtures/nodebug.c
	@mkdir -p build
	gcc -O0 -o $@ $<
	strip $@

run: build fixtures
	./$(BIN) -project . -assets-dir internal/assets/web -v

clean:
	rm -f $(BIN)
	rm -rf build
