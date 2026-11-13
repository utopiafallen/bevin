BIN := bin/bevin

.PHONY: build build-linux build-windows run test tidy clean

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

tidy:
	go mod tidy

clean:
	rm -rf bin
