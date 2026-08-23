.PHONY: build test test-browser test-codex-tui test-claude-hook test-all run tidy

VERSION ?= dev

build:
	go build -ldflags="-X main.version=$(VERSION)" -o iris ./cmd

test:
	go test ./...

test-browser: build
	node tests/browser_e2e.mjs

test-codex-tui: build
	node tests/codex_tui_e2e.mjs

test-claude-hook: build
	node tests/claude_hook_e2e.mjs

test-all: test test-browser

run:
	go run ./cmd

tidy:
	go mod tidy
