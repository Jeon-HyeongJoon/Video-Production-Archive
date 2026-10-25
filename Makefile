.PHONY: test requirements requirements-lock ontology verify

test:
	go test ./...

requirements:
	go run ./cmd/reqcheck

requirements-lock:
	go run ./cmd/reqcheck -update-lock

ontology:
	go run ./cmd/ontocheck

verify: requirements ontology test
