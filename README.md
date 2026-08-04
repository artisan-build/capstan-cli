# capstan-cli

The Go command-line client for the **Capstan** ecosystem server — the fork-and-deploy AI ecosystem
server (auth + gated artifact host, and eventually the local always-on runner).

## Slice-1 verbs

- `capstan login` — authenticate against a Capstan server and store a durable token in your environment
  so agents can act without ever handling the token themselves. Loopback browser flow by default;
  `--device` for a headless device-code flow.
- `capstan artifact create --file <path> [--visibility org|signed] [--team <slug>] [--expires <dur>]` —
  upload an artifact to the Capstan server and print its share URL. This is how agents publish
  team-visible artifacts instead of using their harness's built-in artifact tool.

## Status

Pre-launch. Built via the Capstan multi-agent build loop; see `.solo/workflow.md`.

Server side: `artisan-build/capstan` (private). Design of record lives in the brain metaproject
(`ideas/ecosystem/`, decisions D24 / D26 / D27).
