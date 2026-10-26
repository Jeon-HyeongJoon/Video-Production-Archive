.PHONY: build test requirements requirements-lock ontology visual-deps dashboard-golden-update verify

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

visual-deps:
	python3 -m pip install -r requirements-visual.txt
	python3 -m playwright install chromium

dashboard-golden-update:
	UPDATE_DASHBOARD_GOLDEN=1 go test -count=1 ./internal/adapter/httpcontrol -run '^TestControlHTTPServesSQLiteDashboardAndSearchOnlyToAllowedNetwork$$'

verify: requirements ontology test
