# capstan-cli

The Go command-line client for the **Capstan** ecosystem server — the fork-and-deploy AI ecosystem
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

## Status

Pre-launch. Built via the Capstan multi-agent build loop; see `.solo/workflow.md`.

Server side: `artisan-build/capstan` (private). Design of record lives in the brain metaproject
(`ideas/ecosystem/`, decisions D24 / D26 / D27).
