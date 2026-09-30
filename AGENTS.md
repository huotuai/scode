# AGENTS.md

## Project overview
`scode` is a lean Go coding agent (CLI + TUI + headless server + Electron desktop client) built on the architecture of [pi](https://pi.dev), iterating independently of upstream. Primary language is Go 1.26 (module `scode`); the desktop client under `desktop/` is Electron + React 19 + TypeScript + Vite. Key Go deps: bubbletea v2/lipgloss v2 (TUI), modelcontextprotocol/go-sdk (MCP client), doublestar (glob matching).

## Building and running
From the repository root:

```sh
go build ./...                                  # compile check all packages
go build -o dist/scode.exe ./cmd/scode          # plain binary
build.bat                                       # Windows: version-stamped build into dist/
./build.sh                                      # Unix: same, via git rev-parse + date
```

Run modes (binary or `go run ./cmd/scode`):

```sh
scode -p "task"            # print mode (one-shot)
scode                      # TUI (default interactive, bubbletea)
scode --repl               # legacy line REPL
scode serve                # headless stdio NDJSON JSON-RPC service (desktop backend)
scode --resume <id> -p "…" # resume a session
```

Desktop client:

```sh
cd desktop && npm install
npm run dev                 # Vite HMR + Electron dev mode
npm run build && npm start  # production (loads dist-renderer/)
```

Backend binary resolution for desktop: `SCODE_BIN` env > `dist/scode.exe` > PATH.

## Testing
```sh
go test ./...                                        # full suite
go test ./internal/agent                             # one package
go test ./internal/llm -run TestPrefixStability -v   # single test
```
Tests live next to sources as `*_test.go`; platform-specific tests use build-tag suffixes (`_windows_test.go`, `confine_unix_test.go`). There is no test setup for the desktop client.

## Linting / formatting
No linter is configured (no golangci-lint config, no eslint). Use `gofmt`/`go vet ./...`. Desktop has script-based checks only: `npm run typecheck` (tsc over `renderer/tsconfig.json`) and `npm run check` (`node --check` on main/preload).

## Code style
- Standard Go layout and naming; `gofmt`-formatted; imports grouped stdlib / external / `scode/internal/...`.
- **Code comments and identifiers are in English**; user-facing docs (README, design docs, progress notes) are in Chinese. Match the file you're editing.
- Comments frequently cite the upstream behavior being mirrored, e.g. `// pi's --skill <path>`, `// pi edit.ts schema` — keep this convention when porting/aligning behavior.
- Errors flow into streams/events (LLM failures become `error` events with `stopReason=error`), never panic; returned errors use `fmt.Errorf` with context.
- Tool schemas are hand-written `json.RawMessage` literals inside `Decl()`; tool argument parsing tolerates and repairs common malformed model output.
- React/TS renderer state lives in zustand stores under `desktop/renderer/src/store/`; styling via CSS variables (light/dark themes).

## Architecture
- `cmd/scode/main.go` — entry point: flag parsing, dispatches to `serve` / TUI / REPL / print mode; also handles the sandbox launcher self-re-exec (`scode __sandbox_landlock`).
- `internal/llm` — provider-neutral `Message`/`Event` model plus protocol adapters (`anthropic/`, `openaic/`, `responses/`, `google/`); wire-format conversion happens only at the provider boundary.
- `internal/agent` — agent loop, hooks (before/afterToolCall, transformContext, prepareRequest), compaction, steering, auto-title.
- `internal/tools` — built-in tools: read/write/edit/bash/grep/find/ls, background bash, truncation.
- `internal/session` — append-only JSONL session storage (pi v2 tree format); system prompt/tool decls are NOT persisted, rebuilt at load.
- `internal/prompt`, `internal/memory` — system prompt assembly (AGENTS.md, transcript section deltas).
- `internal/sandbox` — file-effect sandbox policy (`read-only`/`workspace-write`/`danger-full-access`) + OS runners: Windows restricted-token ACL, Linux bwrap→landlock, macOS seatbelt; fail-closed everywhere.
- `internal/permission` — rule engine, deny > ask > allow over settings.json `permissions`.
- `internal/planmode`, `internal/plantrack` — plan mode (write ops hard-rejected except `.scode/plan/**`).
- `internal/skills` — Agent Skills discovery (`.scode/skills`, `.agents/skills`, etc.).
- `internal/mcp` — MCP client bridge (`~/.scode/mcp.json`, tools registered as `mcp__<server>__<tool>`).
- `internal/config` — settings.json loading (providers, retry, sandbox, permissions).
- `internal/cli`, `internal/tui` — REPL and bubbletea TUI frontends.
- `internal/server` — `scode serve`: stdio NDJSON JSON-RPC 2.0 session service (stdout = frames only, logs to stderr).
- `internal/subagent` — sub-agent task delegation.
- `desktop/` — Electron shell (`src/main.js`, `src/preload.js`) spawning `scode serve`; React+TS renderer in `desktop/renderer/`; packaging in `desktop/scripts/package.mjs`.

Flow: UI (TUI/REPL/serve→desktop) → `agent` loop → `llm` provider → `tools` (gated by `permission` + `sandbox`) → `session` JSONL (single source of truth).

## Notes
- Primary dev platform is **Windows**: `cmd.exe`-flavored tooling, `dist/scode.exe` output, `_windows.go` build-tag files throughout; sandbox runners for Linux/macOS exist but the hot path is Windows.
- `.gitattributes` pins `*.bat`/`*.cmd` to CRLF and `*.sh` to LF — do not normalize.
- Reference snapshot of upstream pi lives at `E:\ai\harness\pi` (see README); key behavioral divergences are documented in source comments and `docs/design-permission-plan-desktop.md`.
- Files named `NUL` / `desktop/nul` exist from accidental Windows redirection (`> NUL`); they are not meaningful artifacts.
- Never put volatile content (dates, git status) into the system prompt or tool declarations — prefix byte-stability of the transcript is a core design pillar (KV cache hits); pass such data via bash tool env vars instead.
- Bash sandbox is fail-closed: with no runner backend or unresolved policy, commands are refused — never bypass this.
- `jindu.md` is a running progress/decision log in Chinese.
