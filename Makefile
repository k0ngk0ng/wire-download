SHELL := /bin/sh
ROOT := $(CURDIR)
export GOCACHE := $(ROOT)/.cache/go-build
export GOPATH := $(ROOT)/.cache/go
export GOTMPDIR := $(ROOT)/.cache/tmp
export TMPDIR := $(ROOT)/.cache/tmp

.PHONY: build test check release
build:
	@mkdir -p .cache/tmp bin
	go build -trimpath -ldflags='-s -w' -o bin/wirectl-download ./cmd/wirectl-download
	go build -trimpath -ldflags='-s -w' -o bin/wirectl ./cmd/wirectl
test:
	@mkdir -p .cache/tmp
	go test -race $(TEST_FLAGS) ./...
	if test -d ../wirectl; then go -C ../wirectl test -race $(TEST_FLAGS) ./...; fi
check: test
	go vet ./...
	if test -d ../wirectl; then go -C ../wirectl vet ./...; fi
release:
	sh scripts/release.sh
