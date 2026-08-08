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

# Where `gem install --user-install` puts executables. Only `make docs` needs it.
GEM_BIN := $(shell ruby -e 'puts Gem.user_dir' 2>/dev/null)/bin

.PHONY: all build test test-integration test-fuzz lint fmt vet fixtures run docs docs-check docs-images vendor-verify clean

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

# minsym is the in-between case: no debug info, but not stripped either, so its
# symbols survive in the ELF table with an address and no type. That is what a
# release firmware image looks like.
build/minsym: testdata/fixtures/minsym.c
	@mkdir -p build
	gcc -O0 -o $@ $<

run: build fixtures
	./$(BIN) -project . -assets-dir internal/assets/web -v

# Preview the documentation site at http://127.0.0.1:4000.
#
# GitHub builds the published site, so this is only for seeing a change before
# pushing it. --baseurl '' because the site is served from /gdb-wui there and
# from the root here; without it every link and image 404s locally.
#
# The user gem directory is put on PATH here rather than left to the reader:
# `gem install --user-install` does not touch PATH, so the usual outcome is a
# correct install and a "command not found".
docs:
	@PATH="$(GEM_BIN):$$PATH" command -v jekyll >/dev/null || { \
	  echo "jekyll not found. Install it with:"; \
	  echo "  gem install --user-install --no-document bundler jekyll jekyll-remote-theme jekyll-relative-links"; \
	  exit 1; }
	cd docs && PATH="$(GEM_BIN):$$PATH" jekyll serve --baseurl '' --livereload

# Build the site and follow every internal link in it.
#
# Against the built HTML, not the Markdown: `[Install](install.md)` points at a
# file that exists, so a source-level check passes while the site 404s, because
# the built page is install.html.
docs-check:
	@PATH="$(GEM_BIN):$$PATH" command -v jekyll >/dev/null || { \
	  echo "jekyll not found; see the docs target"; exit 1; }
	cd docs && PATH="$(GEM_BIN):$$PATH" jekyll build --baseurl ''
	python3 scripts/check-links.py

# The screenshots the site uses. Regenerates every image; see
# scripts/screenshots/README.md.
docs-images:
	node scripts/screenshots/capture.mjs

clean:
	rm -f $(BIN)
	rm -rf build
