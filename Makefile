install:
	go test ./...
	go build -trimpath -ldflags "-s -w" -o $(HOME)/.local/bin/codex-watchdog ./cmd/codex-watchdog

.PHONY: install
