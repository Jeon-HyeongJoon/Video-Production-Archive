.PHONY: build test requirements requirements-lock ontology verify

build:
	mkdir -p bin
	go build -o bin/video-archive-control ./cmd/video-archive-control
	go build -o bin/video-archive-worker ./cmd/video-archive-worker

test:
	go test ./...

requirements:
	go run ./cmd/reqcheck

requirements-lock:
	go run ./cmd/reqcheck -update-lock

ontology:
	go run ./cmd/ontocheck

verify: requirements ontology test
