# Everything here works from a clean checkout with nothing but the go toolchain.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint run dev watch debug install tidy dist clean

build:
	go build -ldflags '$(LDFLAGS)' -o workmux ./cmd/workmux

test:
	go test ./...

# gofmt -l lists files that need formatting; any output is a failure.
lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt:"; echo "$$out"; exit 1; fi
	go vet ./...

# Run straight from source against any project — no build, no install.
#   make dev ROOT=~/code/my-drupal-site
# The frontend is served from internal/web/dist, so editing it is a refresh, and
# every git/docker/agent subprocess is logged with its duration and exit code.
ROOT ?= .
PORT ?= 4315
dev:
	go run ./cmd/workmux --root $(ROOT) --port $(PORT) --dev

# Same, restarting whenever a .go file changes. Needs watchexec or entr; without
# either it says so instead of pretending.
watch:
	@if command -v watchexec >/dev/null; then \
		watchexec -e go -r -- go run ./cmd/workmux --root $(ROOT) --port $(PORT) --dev; \
	elif command -v entr >/dev/null; then \
		find . -name '*.go' | entr -r go run ./cmd/workmux --root $(ROOT) --port $(PORT) --dev; \
	else \
		echo "install watchexec (brew install watchexec) or entr, then: make watch ROOT=..."; \
		exit 1; \
	fi

# Under a debugger, breakpoints and all: dlv from the delve project.
debug:
	dlv debug ./cmd/workmux -- --root $(ROOT) --port $(PORT) --dev

# Put it on PATH as the real thing (~/go/bin/workmux), which is what `bin/dev
# serve` in a project will find.
install:
	go install -ldflags '$(LDFLAGS)' ./cmd/workmux

run: build
	./workmux --root $(ROOT)

tidy:
	go mod tidy

# Cross-compiled release binaries. No cgo, so these are fully static.
dist: clean
	@mkdir -p dist
	@for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' \
			-o dist/workmux-$$os-$$arch ./cmd/workmux || exit 1; \
	done

clean:
	rm -rf dist workmux
