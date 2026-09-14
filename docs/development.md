# Development

```bash
make install     # Go modules, Air, dashboard packages
make dev         # backend and frontend, both hot-reloading
make check       # go vet, go test, tsc
make build       # one binary with the dashboard embedded
```

`make dev` runs the API on `:8787` with [Air](https://github.com/air-verse/air)
watching Go files, and Vite on `:5173` proxying `/api` to it. Both bind
loopback by default; set `HOST=0.0.0.0` on the Vite side and change
`server.host` for the API when another machine on the network needs to reach
them (the API refuses a non-loopback bind without `server.auth_token`, a
dashboard password, or an explicit `server.auth_disabled: true`).

## Targets

| | |
|---|---|
| `make dev` | Both servers |
| `make dev-api` | Backend only |
| `make dev-web` | Frontend only |
| `make check` | Vet, test, typecheck |
| `make test` | Go tests |
| `make smoke` | Load every dashboard route in a real browser |
| `make build` | Production binary |
| `make doctor` | Diagnose the configuration |

## Layout

Go code is in `internal/`, one package per concern, described in
[Architecture](architecture.md). The dashboard is in `web/`.

## Adding things

**A tool** — implement the four-method interface in `internal/tools`, register
it in `register.go`, add it to the toolsets that should have it. See
[Tools](tools.md).

**A command** — one `register(Spec{...}, handler)` in `internal/commands`. It
appears in the terminal, the web chat, and the gateways at once. See
[Commands](commands.md).

**A provider** — implement `llm.Client` in `internal/llm`. Most endpoints are
OpenAI-shaped and need no new adapter, only configuration.

**A dashboard page** — add an entry to `web/src/lib/routeManifest.ts` (the
single manifest `routes.ts` consumes at runtime and `scripts/smoke.mjs` reads
at build time) and the component. Navigation, routing, chrome, and the smoke
walk all follow from that entry; the page itself contains content only.

**A store method** — add it to the `Store` interface and implement it once in
`internal/store/sql.go`. SQLite and Postgres share the implementation; only
search differs.

**A locale** — dictionaries are code-split. Add a `web/src/lib/locales/<lang>.ts`
file and wire a dynamic import into `LAZY_DICT_LOADERS` in `i18n.tsx`; the
initial bundle keeps shipping English only.

**Chat surface.** The web chat is split so the transcript, the streaming
queue, and the SSE controller each have their own testable module:
`web/src/components/chat/ChatTranscript.tsx` renders the virtualised list,
`web/src/lib/chatTranscript.ts` normalises server events into transcript
state, `web/src/lib/chatStreamQueue.ts` batches high-rate deltas, and
`web/src/lib/chatStreamController.ts` owns the `fetch`-driven SSE stream and
its lifecycle. Their `.test.mjs` siblings run under `make web-test`.

**Agent internals.** `internal/agent/turn_lifecycle.go` owns the per-turn
state machine, `internal/agent/admission.go` gates concurrent sessions
against `max_concurrent_sessions`, and `internal/agent/tool_execution.go`
dispatches tool calls. `internal/agent/harness.go` implements the repetition
guard (with a same-path fingerprint for `write_file`/`edit_file` so repeated
writes to one file with different content still trip it) and verification
plumbing.

**Dev-only samples.** UI prototypes under `web/src/sample/` mount at
`/sample/<name>` in dev only; the loader is guarded by `import.meta.env.DEV`
so production bundles never see the folder and `/sample` 404s to `/`.

## Testing

```bash
make test
go test ./internal/store/ -run TestMemory -v
```

The browser tests drive a real Chromium and skip when none is installed. The
store tests run against SQLite in a temporary directory; set
`TEST_POSTGRES_DSN` to also exercise them against Postgres. That helper
creates a fresh, uniquely named schema per test on the target database and
drops only that schema on cleanup — the base database and its `public`
schema are never touched, so pointing this at a shared or dev Postgres is
safe.

The dashboard is checked by loading every route in a headless browser, which
is what `make smoke` does. That exists because two of the worst bugs so far
passed `tsc` and `go vet` cleanly and blanked the entire page: a hook called
from inside an effect, and the server bouncing SPA routes to `./`. Static
analysis cannot see either. The smoke run walks the shared route manifest
(`web/src/lib/routeManifest.ts`) across desktop and mobile viewports against
isolated fixtures, and boots the server against a stubbed provider that
refuses any real completion — a page that fires a background chat, embedding
or image request fails the whole pass.

## Style

**Go.** Standard library first. `gofmt`. Errors wrapped with context, not
swallowed. Comments explain why, not what — a comment restating the code is
noise, and one explaining a non-obvious decision is worth several lines.

**TypeScript.** No semicolons, single quotes, 100 columns. Function components
with hooks. Tailwind utilities, no CSS files beyond the token definitions.

**Both.** Match the code around you. If a file does something one way, do it that
way, or change the whole file.

## Commits

The subject says what changed, in the imperative, under about 70 characters. The
body says why — the diff already says what. Where a decision was not obvious,
say what the alternative was and why it lost.
