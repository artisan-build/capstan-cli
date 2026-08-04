# Workflow — capstan-cli

Project profile for the `multi-agent-build` skill. The coordinator agent reads this FIRST.
Design of record: brain metaproject `ideas/ecosystem/README.md` (D24 auth CLI, D26 stack split =
Go CLI/runner, D27 artifact creation = CLI-verb transport). This CLI is the START of the D8 runner
binary, not a throwaway — build it structured and tested.

## What this is
The Go CLI for the Capstan ecosystem server. Slice-1 verbs:
- `capstan login` — authenticate; plant a durable token in the user's environment/config so agents
  never handle the token themselves (the `cli-is-agent-credential-boundary` principle).
- `capstan artifact create` — upload an artifact blob to the Capstan server's ingest API (D27); the
  agent invokes this verb instead of its harness's native artifact/publish affordance.

The server side already exists (private repo `artisan-build/capstan`): PR4 auth (Sanctum PAT + loopback
authorize + RFC-8628 device flow + `ResolveApiActor` + `GET /api/v1/me`) and PR5 artifact ingest.

## Phase & mode
- phase: pre-launch
- default mode: A-autonomous (merge each PR on green CI; the loop's own review+judge are the review)
- merge method: `gh pr merge --squash --auto` — the coordinator merges each PR on green CI (standard
  Mode A). No stray resident agent in this fresh repo, so no hand-off dance.

## Hard gate (green before review; coordinator verifies on the committed SHA, clean tree)
- command: `go build ./... && go vet ./... && go test ./...` and `golangci-lint run`
- the SCAFFOLDING PR establishes this gate + CI and CANNOT itself be CI-gated (verify on a green local
  gate + a driven smoke of the built binary). Gate-on-green applies from the next PR onward.

## CI (the merge gate for Mode A)
- status: PENDING — fresh repo. The scaffolding PR adds `.github/workflows/ci.yml`.
- minimum bar: (1) `go test ./...` (with race where sensible) and (2) static analysis
  (`go vet` + `golangci-lint`). Both must exist before any later PR is CI-gated.
- matrix: pin the Go toolchain via `go.mod` `go` directive; CI uses `actions/setup-go` reading it.

## Stack / conventions
- module: `github.com/artisan-build/capstan-cli`; binary name: `capstan`.
- CLI framework: **cobra** (+ viper for config if needed) — the Go-ecosystem default.
- config/token home: `$XDG_CONFIG_HOME/capstan` (fallback `~/.config/capstan`); NEVER print or log the
  token; store with 0600 perms.
- login: **loopback browser flow primary** (opens browser, captures token on a localhost redirect),
  **device-code (RFC-8628) fallback** via `--device` (headless/SSH). Reuse the server's existing flows.
- HTTP: standard library or a thin client; respect a `--server`/`CAPSTAN_SERVER` base URL
  (default the production ecosystem host); send the PAT as a bearer.
- errors: exit non-zero with a clear stderr message; verbs are scriptable + agent-invocable.

## Dependency install (fresh worktree)
- command: `go mod download`
- Go toolchain: use the version pinned in `go.mod` (install via the system Go / setup-go in CI).

## Toolchain conformance — ride-along rule (STANDING)
Run `gofmt`/`golangci-lint --fix` (or the project's `make fmt`) when finalizing each PR; let formatting
ride along as one isolated commit. Tool-config changes (`.golangci.yml`) get their own PR.

## Ship details
- branch naming: `feat/<slug>`
- PR target repo: `artisan-build/capstan-cli`
- release/split: none yet (later: goreleaser + signed builds, mirroring ballast-cli distribution).

## Harness map (decorrelate by ROLE, not model — only Claude + OpenCode CLIs work in this Solo env)
- implementer: OpenCode (resolve id at spawn via list_agent_tools)
- quality reviewer: Claude
- acceptance judge: Claude

## Plan & coordination
- plan location: the PRD scratchpad assembled at build kickoff (brain hands it over).
- run-log: the coordinator's Solo scratchpad, appended at every transition.
