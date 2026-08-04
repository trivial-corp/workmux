# Everything here works from a clean checkout with nothing but the go toolchain.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint run tidy dist clean

build:
	go build -ldflags '$(LDFLAGS)' -o workmux ./cmd/workmux

test:
	go test ./...

# gofmt -l lists files that need formatting; any output is a failure.
lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt:"; echo "$$out"; exit 1; fi
	go vet ./...

run: build
	./workmux --root .

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
