.PHONY: build test race stress vet check

build:
	go build -o bin/loopctl ./cmd/loopctl

test:
	go test ./...

race:
	go test -race ./...

stress: build
	bash tests/stress.sh ./bin/loopctl

vet:
	go vet ./...

check: test race stress vet build
