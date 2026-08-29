# Kindle builds are static and dependency-free: no cgo, no libc version to
# match, one file to copy onto the device.
GOFLAGS := CGO_ENABLED=0
LDFLAGS := -s -w

.PHONY: all kindled arm64 fakepixel test vet fmt clean

all: kindled

## kindled: build for a modern Kindle (32-bit ARM userland)
kindled:
	$(GOFLAGS) GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" -o kindled .

## arm64: build for a 64-bit ARM Kindle userland
arm64:
	$(GOFLAGS) GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o kindled-arm64 .

## fakepixel: build the stand-in server for testing without a phone
fakepixel:
	go build -o fakepixel ./cmd/fakepixel

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -f kindled kindled-arm64 fakepixel
