.PHONY: build test install vet

build:
	go build -o bin/cs ./cmd/cs

test:
	go test -race ./...

vet:
	go vet ./...

install:
	go install ./cmd/cs
