BINARY  := harness
PKG     := ./cmd/harness
VERSION ?= dev-$(shell git rev-parse --short HEAD 2>/dev/null || echo local)
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: build test vet fmt check release install clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: fmt vet test build

release:
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64   $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-amd64  $(PKG)
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe $(PKG)

install: build
	go install -ldflags "$(LDFLAGS)" $(PKG)

clean:
	rm -f $(BINARY) $(BINARY).exe; rm -rf dist
