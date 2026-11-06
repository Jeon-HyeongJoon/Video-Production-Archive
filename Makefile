.PHONY: build test requirements requirements-lock ontology visual-deps archive-workspace-golden-update archive-workspace-golden-evidence verify

build:
	mkdir -p bin
	go build -o bin/archive-control ./cmd/archive-control
	go build -o bin/archive-worker ./cmd/archive-worker
	go build -o bin/archivist-host ./cmd/archivist-host
	go build -o bin/archivist-fixture ./cmd/archivist-fixture
	go build -o bin/archive-secret ./cmd/archive-secret

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

archive-workspace-golden-update:
	UPDATE_ARCHIVE_WORKSPACE_GOLDEN=1 go test -count=1 ./internal/adapter/httpcontrol -run '^(TestArchiveOrganizationWorkspaceMatchesOperatingSystemGolden|TestArchiveWorkspaceJobManagementWithArchivistPaneMatchesOperatingSystemGolden|TestWorkspaceRoutesJobListsAndCompletedResultsToPersistentJobDetail)$$'

archive-workspace-golden-evidence:
	./deploy/run-archive-workspace-golden-evidence.sh

verify: requirements ontology test
