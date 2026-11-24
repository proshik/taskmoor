# taskmoor — build and checks.

bin := "bin/taskmoor"

default: check

# gofmt, go vet, golangci-lint when installed, go test
check: fmt-check vet lint test

# fail when a file is not gofmt'ed
fmt-check:
    @out="$(gofmt -l .)"; if [ -n "$out" ]; then echo "gofmt needed:"; echo "$out"; exit 1; fi

vet:
    go vet ./...

# golangci-lint runs only when it is installed
lint:
    @if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint is not installed; skipped"; fi

test:
    go test ./...

build:
    go build -o {{bin}} ./cmd/taskmoor

fmt:
    gofmt -w .

# the release archives in dist/, as the release workflow builds them
dist version:
    scripts/dist.sh {{version}}
