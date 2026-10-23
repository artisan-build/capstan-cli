# capstan-cli

The Go command-line client for the **Capstan** ecosystem server, the fork-and-deploy AI ecosystem
server (auth + gated artifact host, and eventually the local always-on runner).

## Slice-1 verbs

- `capstan version` — print the built CLI version. Development builds report `dev`; release builds can
  override it with Go linker flags.
- `capstan login` — authenticate against a Capstan server and store its URL plus a durable token in
  your environment so agents can act without ever handling the token themselves. Pass `--server`, set
  `CAPSTAN_SERVER`, or enter the server URL when prompted. Loopback browser flow by default;
  `--device` for a headless device-code flow.
- `capstan artifact create --file <path> [--visibility org|signed] [--team <slug>] [--expires <dur>]` —
  upload an artifact to the Capstan server and print its share URL. This is how agents publish
  team-visible artifacts instead of using their harness's built-in artifact tool.
- `capstan runners check [--config <path>]` — read local launchd runner health and reconcile deduped,
  durable Markdown findings in the configured inbox. Findings still exit 0; configuration, reader,
  locking, and filesystem failures exit non-zero.
- `capstan work drain [--config <path>]` — run one recovery, renewal, claim, and launch tick for
  Capstan Work. A scheduler starts each tick; the command does not remain resident.

## Build

```sh
go build ./...
go test ./...
```

To build a binary with an explicit version:

```sh
go build -ldflags "-X github.com/artisan-build/capstan-cli/cmd.Version=0.1.0" -o capstan .
./capstan version
```

## Runner configuration

Local runners are configured in `$XDG_CONFIG_HOME/capstan/config.yaml`, falling back to
`$HOME/.config/capstan/config.yaml`. Both `inbox.path` and `state.path` are required writable
directories. Each launchd runner needs a unique `name` and launchd `label`. Names are 1-64 ASCII
letters, digits, dots, underscores, or hyphens, must start with a letter or digit, and are unique
case-insensitively.

`expected_cadence` is optional: omitting it defines an on-demand runner with no staleness checks. For
periodic runners, `expected_cadence` must be positive and `staleness_threshold` defaults to it. A
threshold cannot be configured without a cadence. Paths beginning with `~/` are expanded for
`inbox.path`, `state.path`, and `activity_source`.

Runner health is based on launchd's exit status and reason plus freshness, never on stdout logs.
Freshness uses `activity_source` mtime when configured, otherwise the change in launchd's monotonic
`runs` counter between checks. See [`docs/config.example.yaml`](docs/config.example.yaml) for a fully
commented configuration.

`capstan runners check` writes one file per runner and failure kind. Repeated findings update only
the YAML frontmatter so human notes in the Markdown body survive. Recovery resolves the existing
file without deleting it, and recurrence reopens that same file. The check is strictly read-only:
it never starts, stops, restarts, reloads, or modifies a configured runner.

The Markdown evidence section records the first observation and is not regenerated, preserving any
human edits byte-for-byte. The frontmatter `evidence` mapping carries the current observed values.

### Scheduling with launchd

The sample [`docs/launchd/com.artisan-build.capstan-runners-check.plist`](docs/launchd/com.artisan-build.capstan-runners-check.plist)
runs the check every five minutes using `StartInterval`. Before installing it, change
`/opt/homebrew/bin/capstan` to the absolute path of the installed binary if necessary (Intel Homebrew
typically uses `/usr/local/bin`). The command uses the default XDG config; add `--config` and an
absolute config path to `ProgramArguments` if needed. The sample sends summaries and operational
errors to `/tmp/capstan-runners-check.stdout.log` and `/tmp/capstan-runners-check.stderr.log`.

Install the sample as a user LaunchAgent:

```sh
mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Herd/brain/inbox" "$HOME/.local/state/capstan"
cp docs/launchd/com.artisan-build.capstan-runners-check.plist \
  "$HOME/Library/LaunchAgents/com.artisan-build.capstan-runners-check.plist"
launchctl bootstrap "gui/$(id -u)" \
  "$HOME/Library/LaunchAgents/com.artisan-build.capstan-runners-check.plist"
```

This repository intentionally ships no installer for the monitor.

## Work drain

`capstan work drain` defaults to `$XDG_CONFIG_HOME/capstan/work.json`, falling back to
`$HOME/.config/capstan/work.json`. The file must have mode `0600` or stricter. Start with
[`docs/work.example.json`](docs/work.example.json), copy it into the Capstan config directory, and
replace its example values and absolute command paths. Relative paths are resolved from the config
file's directory.

There is no daemon.

The credential file contains only a bound `capstan.work.drain` bearer and must also have mode `0600`
or stricter. The drain reads it after taking the non-blocking overlap lock. It sends the bearer in the
Authorization header and never writes it to state or output. `capstan_url` must use HTTPS except for
localhost and loopback development.

Each role needs an explicit provider and an argv array in `role_commands`; shell command strings are
not accepted. Prompt and command templates may use `{config_dir}`, `{fence}`, `{item_id}`,
`{launch_nonce}`, `{links_file}`, `{prompt_file}`, `{role}`, `{seat_fence}`, `{session_ref}`,
`{task_id}`, and `{work_dir}`. Unknown placeholders stop configuration loading. Signed URLs are kept
only in each item's private `links.json`, so prompts should tell the worker to read `{links_file}`
instead of copying URLs into the prompt or command line.

Before it claims anything, one tick loads the durable record, renews every recorded item in batches
of at most 25, stops local sessions that have lost authority, releases live claims whose sessions
have died, and resumes partial `delivered` and `routed` transitions. Load or memory above the
configured ceiling sends zero leaf and orchestrator capacity while still reporting held items and
accepting server-selected repair directives. A failed health measurement stops the tick without
claiming.

Leaf `max_concurrent` and `orchestrator_capacity` are separate, but their sum cannot exceed the
server batch limit of 25. Every claim tick reports both capacities, the current held leaf count,
roles, capabilities, role providers, and fenced held items. The drain runs only `send_enter`,
`resend_pointer`, or `respawn` directives returned by that tick. Directive outcomes are durable and
reported on the next tick; replay does not execute a completed directive twice.

The generic launcher runs configured argv without a shell behind a local gate. It records recovery
state, sends `delivered` with the server nonce and opaque session reference, sends `routed`, and then
opens the gate. This adapter cannot send interactive Enter or pointer repairs, so those directives
report failure. PR7 will add the Solo stdio launcher, `capstan work setup`, and the live self-test;
this command does not contain Solo-specific behavior yet.

Run one tick from a scheduler such as launchd, systemd, or cron:

```sh
/absolute/path/to/capstan work drain
```

Run it once per minute and use absolute paths in the scheduler. Overlapping runs exit successfully
without reading the credential or making an HTTP request. Keep the config, credential, state, lock,
and item directories under a private user-owned directory. The global state file contains fences,
phases, nonces, and opaque session references. No bearer, prompt, or signed URL enters it. Item prompt and
link files use mode `0600`; their parent directories use mode `0700`.

## Status

Pre-launch. Built via the Capstan multi-agent build loop; see `.solo/workflow.md`.

Server side: `artisan-build/capstan` (private). Design of record lives in the brain metaproject
(`ideas/ecosystem/`, decisions D24 / D26 / D27).
