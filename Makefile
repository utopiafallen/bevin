BIN := bin/bevin

.PHONY: build build-linux build-windows run test check tidy clean

build:
	go build -o $(BIN) ./cmd/bevin

build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BIN)-linux-amd64 ./cmd/bevin

build-windows:
	GOOS=windows GOARCH=amd64 go build -o $(BIN)-windows-amd64.exe ./cmd/bevin

run:
	go run ./cmd/bevin

test:
	go test ./...

# check: vet under BOTH the host GOOS and windows — the windows-tagged
# files (the HCS check, the CPUID asm stub, the windows tests) only
# compile under GOOS=windows, so host-only vetting misses their bugs
# (the CPUID asm ABI bug slipped through this way).
check:
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin
