include tools/versions.env

GO_TOOLCHAIN := $(strip $(shell awk '$$1 == "toolchain" { print $$2 }' go.mod))
export GOTOOLCHAIN := $(GO_TOOLCHAIN)

GOLANGCI_LINT := .cache/tools/golangci-lint/$(GOLANGCI_LINT_VERSION)/golangci-lint

.PHONY: run check live-online-resume toolchain-check quality-gate-contract-check module-boundary-check ci-check fmt-check mod-check lint test coverage race vuln generate-check

run:
	go run ./cmd/boss-job-agent

test:
	go test -count=1 ./...

live-online-resume:
	BOSS_ONLINE_RESUME_LIVE=1 go test -count=1 -tags=live ./internal/adapters/boss -run '^TestOnlineResumeLiveReadsTheAuthenticatedBossResume$$' -v

check:
	$(MAKE) toolchain-check
	$(MAKE) quality-gate-contract-check
	$(MAKE) fmt-check
	$(MAKE) mod-check
	$(MAKE) module-boundary-check
	$(MAKE) ci-check
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) coverage
	$(MAKE) race
	$(MAKE) vuln
	$(MAKE) generate-check

toolchain-check:
	./scripts/check-tool-versions.sh

quality-gate-contract-check: toolchain-check
	./scripts/check-quality-gate-contracts.sh

module-boundary-check:
	bash -n scripts/*.sh
	./scripts/check-module-boundaries.sh

ci-check:
	./scripts/check-ci.sh

fmt-check: toolchain-check
	./scripts/check-format.sh

mod-check: toolchain-check
	go mod tidy -diff
	go mod verify
	go -C tools mod tidy -diff
	go -C tools mod verify

$(GOLANGCI_LINT):
	./scripts/install-tool.sh golangci-lint

lint: toolchain-check $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) config verify
	go vet ./...
	$(GOLANGCI_LINT) run

coverage: toolchain-check
	./scripts/check-coverage.sh

race: toolchain-check
	go test -count=1 -race ./...

vuln: toolchain-check
	@tool="$$(go -C tools tool -n govulncheck)"; "$$tool" ./...

generate-check: toolchain-check
	./scripts/check-generated.sh
