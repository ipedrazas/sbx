VERSION    ?= 0.1.10
GIT_SHA    ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG        := github.com/ipedrazas/sbx/internal/version
LDFLAGS    := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).GitSHA=$(GIT_SHA) -X $(PKG).BuildTime=$(BUILD_TIME)
COVERAGE_MIN ?= 45

.PHONY: build run test cover lint docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/sbx ./cmd/sbx

run: build
	./bin/sbx --port 9820

test:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {sub("%","",$$3); print $$3}'); \
	  awk -v t=$$total -v m=$(COVERAGE_MIN) 'BEGIN { if (t+0 < m+0) { printf "coverage %.1f%% < %d%%\n", t, m; exit 1 } }'

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg GIT_SHA=$(GIT_SHA) --build-arg BUILD_TIME=$(BUILD_TIME) -t sbx:$(VERSION) .

clean:
	rm -rf bin coverage.out
