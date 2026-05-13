# devin-key-manager

Local manager for rotating multiple Devin.ai API keys across sessions, with
context handoff between rotations. Single static binary, web dashboard at
`http://localhost:8765`.

> **Status:** PR-1 — skeleton + encrypted key storage. UI for managing keys
> only. Session orchestration, quota detection, and handoff land in later PRs.

## How it works (target architecture)

```
Windows 10
+--------------------------------------------------+
| devinmgr.exe                                     |
| |- HTTP server :8765 (HTMX + Tailwind UI)        |
| |- Background scheduler                          |
| |  |- session poller                             |
| |  |- cooldown ticker                            |
| |  '- handoff orchestrator                       |
| |- Devin API client (Bearer <key>)               |
| |- SQLite (devinmgr.db)                          |
| '- ./artifacts/  <- downloaded session outputs   |
+--------------------------------------------------+
```

- **One API key per Devin account/org.** Knowledge, playbooks, blueprints,
  secrets are *per-org*, so they do not transfer across keys. The manager
  carries context across rotations by generating a `HANDOFF.md` in the current
  session and attaching it to the next one.
- **Quota model:** keys cycle through `active -> cooldown_daily (24h) ->
  active -> cooldown_daily -> cooldown_weekly (until end of week)`. Detected
  via HTTP 402/429 from the Devin API or quota-exhausted messages from Devin
  in chat.
- **Storage:** SQLite (`devinmgr.db`) sits next to the binary; API keys are
  encrypted at rest with AES-GCM using a master key auto-generated on first
  run and saved to `.master_key` (file mode `0600`).

## Building

Requirements: Go 1.22+.

```
go build -o devinmgr ./cmd/devinmgr            # linux/macos host
GOOS=windows GOARCH=amd64 go build -o devinmgr.exe ./cmd/devinmgr
```

Or:

```
make build           # current platform
make build-windows   # devinmgr.exe in ./dist/
```

## Running

```
./devinmgr                              # default :8765, ./devinmgr.db
DEVINMGR_ADDR=:9000 ./devinmgr          # custom port
DEVINMGR_DB=/tmp/test.db ./devinmgr     # custom DB path
```

Open `http://localhost:8765/keys` to add your Devin API keys.

## Roadmap

- [x] PR-1: skeleton + encrypted key CRUD
- [ ] PR-2: Devin API client + tasks/sessions UI
- [ ] PR-3: quota-exhaustion detection + 24h cooldown + handoff
- [ ] PR-4: file attachments + artifact download
- [ ] PR-5: Windows `.exe` build pipeline + launcher
