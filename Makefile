BIN     := ecr-prime
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo 0000000)
TS      ?= $(shell date -u +%Y-%m-%dT%H:%M:%S)
BUILDER ?= $(shell whoami 2>/dev/null)@$(shell hostname 2>/dev/null)
URL     := https://github.com/babs/ecr-prime

LDFLAGS := -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.CommitHash=$(COMMIT)' \
	-X 'main.BuildTimestamp=$(TS)' \
	-X 'main.Builder=$(BUILDER)' \
	-X 'main.ProjectURL=$(URL)'

.PHONY: build tidy lint test clean

build: ## static binary
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN) .

tidy:
	go mod tidy

lint:
	golangci-lint run --enable bodyclose,gocritic,gosec,misspell,noctx,revive,unconvert

test:
	go test ./...

clean:
	rm -f $(BIN)
