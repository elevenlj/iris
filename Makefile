.PHONY: build test test-browser test-multi-bot test-feishu-setup test-codex-tui test-claude-hook test-aiden-hook test-all run tidy

VERSION ?= dev

build:
	go build -ldflags="-X main.version=$(VERSION)" -o iris ./cmd

test:
	go test ./...

test-multi-bot:
	IRIS_BROWSER_TEST=1 go test -race ./cmd -run TestBotsBrowserIntegration -v -count=1

test-feishu-setup:
	node tests/feishu_setup_test.cjs

test-browser: build
	node tests/browser_e2e.mjs

test-codex-tui: build
	node tests/codex_tui_e2e.mjs

test-claude-hook: build
	node tests/claude_hook_e2e.mjs

test-aiden-hook: build
	node tests/aiden_hook_e2e.mjs

test-all: test test-browser

run:
	go run ./cmd

tidy:
	go mod tidy
