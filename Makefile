# Everything here works from a clean checkout with nothing but the go toolchain.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test test-race lint run dev watch debug install deploy tidy dist clean

build:
	go build -ldflags '$(LDFLAGS)' -o workmux ./cmd/workmux

test:
	go test ./...

# The dashboard reads git, docker and agent state concurrently; a race there
# surfaces as a wrong number, not a crash, so it needs the detector to find it.
test-race:
	go test -race ./...

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

# Cross-compile for a remote box and install it there, without a Go toolchain or a
# git checkout on the far side:
#
#   make deploy HOST=homelab                    # ~/.local/bin/workmux
#   make deploy HOST=root@nas DEST=/usr/local/bin
#
# The architecture comes from the box itself — guessing it is the one thing that
# silently produces a binary that won't run.
DEST ?= ~/.local/bin
deploy:
	@[ -n "$(HOST)" ] || { echo "usage: make deploy HOST=[user@]host [DEST=~/.local/bin]"; exit 1; }
	@set -e; \
	os=$$(ssh $(HOST) uname -s | tr '[:upper:]' '[:lower:]'); \
	arch=$$(ssh $(HOST) uname -m); \
	case "$$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; \
	  *) echo "no build for $$arch"; exit 1 ;; esac; \
	echo "  building for $$os/$$arch"; \
	mkdir -p dist; \
	CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
	  -o dist/workmux-$$os-$$arch ./cmd/workmux; \
	echo "  installing to $(HOST):$(DEST)/workmux"; \
	ssh $(HOST) "mkdir -p $(DEST)"; \
	scp -q dist/workmux-$$os-$$arch $(HOST):$(DEST)/workmux.new; \
	ssh $(HOST) "mv $(DEST)/workmux.new $(DEST)/workmux && chmod +x $(DEST)/workmux && $(DEST)/workmux --version"; \
	echo "  now on the box:  cd /your/repo && workmux init"

clean:
	rm -rf dist workmux
