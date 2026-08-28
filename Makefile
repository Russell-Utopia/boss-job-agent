.PHONY: run test check

run:
	go run ./cmd/boss-job-agent

test:
	go test ./...

check:
	go vet ./...
	go test ./...
