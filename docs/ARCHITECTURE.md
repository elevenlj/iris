# Iris Architecture

This implementation follows the supplied product specification:

- Go HTTP server with embedded static assets.
- xterm.js browser terminal connected through WebSocket.
- PTY-backed shell sessions with SQLite persistence.
- Session lifecycle states: `running`, `waiting`, `exited`, `failed`.
- Waiting notifications through Lark App API when configured.
- Lark reply bridge entry points for routing text to sessions.
- Image paste upload endpoint scoped by session.
- Quick command CRUD endpoints.

The current code favors small, testable components:

- `internal/session` owns PTY lifecycle, state transitions, output buffers, notification formatting, and Lark helpers.
- `internal/store` owns SQLite schema and persistence.
- `internal/httpapi` owns REST, WebSocket, upload handling, and embedded frontend files.
- `cmd` wires configuration, storage, manager, Lark bridge, and HTTP serving.
