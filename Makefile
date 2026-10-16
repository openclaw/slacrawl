BINARY ?= bin/slacrawl
COMPLETION_DIR ?= dist/completions

.DEFAULT_GOAL := help

.PHONY: help build test test-race fmt fmt-check lint vet vulncheck deadcode workflow-lint tidy-check smoke check run generate-sqlc completion completion-bash completion-zsh snapshot release release-snapshot clean

help:
	@printf '%s\n' \
		'Available targets:' \
		'  help              Print available targets (default).' \
		'  build             Build the CLI into $(BINARY).' \
		'  test              Run the full Go test suite.' \
		'  test-race         Run the full suite with the race detector.' \
		'  fmt               Apply Go formatting.' \
		'  lint              Run vet, vulnerability, and dead-code checks.' \
		'  check             Run every local gate enforced by CI.' \
		'  snapshot          Build credential-free release artifacts.' \
		'  release           Refuse local publishing and print the official CI command.' \
		'  run               Run the CLI (ARGS=...).' \
		'  generate-sqlc     Regenerate sqlc output.' \
		'  completion        Generate bash and zsh completions.' \
		'  clean             Remove local build output.' \
		'  release-snapshot  Alias for snapshot.'

build:
	binary="$(BINARY)"; mkdir -p "$$(dirname -- "$$binary")"; go build -o "$$binary" ./cmd/slacrawl

test:
	GOWORK=off go test -count=1 ./...

test-race:
	GOWORK=off go test -race -count=1 ./...

fmt:
	gofmt -w cmd internal

fmt-check:
	@set -e; \
	changed="$$(gofmt -l .)"; \
	if [ -n "$$changed" ]; then printf 'gofmt wants changes in:\n%s\n' "$$changed"; exit 1; fi

lint: vet vulncheck deadcode workflow-lint

vet:
	GOWORK=off go vet ./...

vulncheck:
	GOWORK=off go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

workflow-lint:
	GOWORK=off go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

deadcode:
	@set -e; \
	output_file="$$(mktemp)"; \
	trap 'rm -f "$$output_file"' EXIT; \
	if ! GOWORK=off go run golang.org/x/tools/cmd/deadcode@v0.52.0 -test ./... > "$$output_file"; then cat "$$output_file"; exit 1; fi; \
	if [ -s "$$output_file" ]; then cat "$$output_file"; exit 1; fi

tidy-check:
	GOWORK=off go mod verify
	GOWORK=off go mod tidy -diff

smoke:
	@set -e; \
	tmpdir="$$(mktemp -d)"; \
	binary="$$tmpdir/slacrawl"; \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	export SLACRAWL_NO_UPDATE_CHECK=1; \
	GOWORK=off go build -buildvcs=true -ldflags "-X github.com/openclaw/slacrawl/internal/cli.version=ci" -o "$$binary" ./cmd/slacrawl; \
	output="$$("$$binary" --help 2>&1)"; \
	printf '%s\n' "$$output"; \
	printf '%s' "$$output" | grep -q 'Usage of slacrawl:'; \
	printf '%s' "$$output" | grep -q metadata; \
	printf '%s' "$$output" | grep -q tui; \
	test "$$("$$binary" --version)" = ci; \
	metadata="$$("$$binary" metadata --json)"; \
	printf '%s' "$$metadata" | grep -q '"schema_version"'; \
	"$$binary" --config "$$tmpdir/slacrawl.toml" init --db "$$tmpdir/slacrawl.db"; \
	status_output="$$("$$binary" --config "$$tmpdir/slacrawl.toml" status --json)"; \
	printf '%s' "$$status_output" | grep -q '"databases"'; \
	tui_output="$$("$$binary" --config "$$tmpdir/slacrawl.toml" tui --json --limit 1)"; \
	printf '%s' "$$tui_output" | grep -q '^\['; \
	revision="$$(git rev-parse --verify HEAD)"; \
	"$$binary" --config "$$tmpdir/slacrawl.toml" --json import testdata/slackdump/f7319928/database/export --workspace TTEST; \
	umask 077; \
	printf '%s\n' '{"workspace_id":"TTEST","workspace_label":"smoke","channels":[{"channel_id":"CPUBLIC","label":"public"}],"messages":[{"channel_id":"CPUBLIC","ts":"1767312000.000002","text":{"mode":"keep","replacement":null}}]}' > "$$tmpdir/selection.json"; \
	"$$binary" --json export prepare --db "$$tmpdir/slacrawl.db" --selection "$$tmpdir/selection.json" --out "$$tmpdir/plan.json"; \
	grep -Fq "\"producer_revision\":\"$$revision\"" "$$tmpdir/plan.json"; \
	"$$binary" --json export build --db "$$tmpdir/slacrawl.db" --plan "$$tmpdir/plan.json" --out "$$tmpdir/projection"; \
	test -s "$$tmpdir/projection/manifest.json"; \
	printf '%s\n' '{"channel_id":"CPUBLIC","ts":"1767312000.000002","user_id":"UME","text":"slackdump-public-standalone","thread_ts":"","edited_ts":""}' | cmp - "$$tmpdir/projection/messages.jsonl"; \
	"$$binary" --json export verify --db "$$tmpdir/slacrawl.db" --plan "$$tmpdir/plan.json" --dir "$$tmpdir/projection"

check: tidy-check fmt-check lint test-race smoke snapshot

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
	@./scripts/release.sh

snapshot:
	$${GORELEASER:-goreleaser} release --snapshot --clean --skip=publish

release-snapshot: snapshot

clean:
	rm -f -- "$(BINARY)"
	rm -rf -- "$(COMPLETION_DIR)"
