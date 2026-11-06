BIN := bin/alcyona

.PHONY: build build-linux build-windows run test tidy clean

build:
	go build -o $(BIN) ./cmd/alcyona

build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BIN)-linux-amd64 ./cmd/alcyona

build-windows:
	GOOS=windows GOARCH=amd64 go build -o $(BIN)-windows-amd64.exe ./cmd/alcyona

run:
	go run ./cmd/alcyona

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -rf bin
