BINARY ?= bin/slacrawl
COMPLETION_DIR ?= dist/completions

.PHONY: build test fmt run generate-sqlc completion completion-bash completion-zsh release release-snapshot clean

build:
	binary="$(BINARY)"; mkdir -p "$$(dirname -- "$$binary")"; go build -o "$$binary" ./cmd/slacrawl

test:
	go test ./...

fmt:
	gofmt -w cmd internal

run:
	go run ./cmd/slacrawl $(ARGS)

generate-sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate

completion: completion-bash completion-zsh

completion-bash:
	mkdir -p "$(COMPLETION_DIR)"
	go run ./cmd/slacrawl completion bash > "$(COMPLETION_DIR)/slacrawl.bash"

completion-zsh:
	mkdir -p "$(COMPLETION_DIR)"
	go run ./cmd/slacrawl completion zsh > "$(COMPLETION_DIR)/_slacrawl"

release:
	@helper="$${MAC_RELEASE_HELPER:-$$HOME/Projects/agent-scripts/skills/release-mac-app/scripts/mac-release}"; \
	GORELEASER="$${GORELEASER:-goreleaser}" "$$helper" codesign-run -- ./scripts/release.sh

release-snapshot:
	$${GORELEASER:-goreleaser} release --snapshot --clean --skip=publish

clean:
	rm -f -- "$(BINARY)"
	rm -rf -- "$(COMPLETION_DIR)"
