# Capstan Work setup

`capstan work drain` is a one-shot command. Solo runs the agents; launchd or systemd starts a fresh
drain tick each minute.

## Configure the machine

Interactive setup lists the Solo projects and agent tools installed on the machine before asking for
bindings:

```sh
capstan work setup
```

Each role needs a numeric Solo project id, a numeric agent-tool id, and its harness (`claude`, `codex`,
or `opencode`). The orchestrator has its own binding and capacity slot. Display names are shown for
people, but setup validates and saves the ids.

Automation can provide every value on the command line:

```sh
capstan work setup \
  --non-interactive \
  --capstan-url https://capstan.example.com \
  --team example-team \
  --actor-id 123 \
  --credential-file "$HOME/.config/capstan/credentials/work-drain" \
  --binding builder=12:34:opencode \
  --binding orchestrator=12:35:claude \
  --schedule print
```

The macOS Solo MCP executable defaults to `/Applications/Solo.app/Contents/MacOS/mcp`. Linux defaults
to `/usr/local/bin/solo-mcp`; use `--solo-mcp /absolute/path` when the package installs it elsewhere.
The selected path is stored in the config, so the scheduled command does not depend on a shell PATH.

Setup writes `work.json` and its prompt files atomically in a private directory. The JSON file and
prompt files use mode `0600`; state and item directories use `0700`. The credential is a separate
private file containing a bound `capstan.work.drain` bearer.

## Schedule

`--schedule print` prints a complete launchd plist on macOS or a systemd user service and timer on
Linux. `--schedule install` writes only these dedicated files:

- macOS: `~/Library/LaunchAgents/com.artisan-build.capstan-work.plist`
- Linux: `~/.config/systemd/user/capstan-work.service` and `capstan-work.timer`

Install mode refuses to replace unrelated content at those paths. It activates the named launchd job
or runs `systemctl --user enable --now capstan-work.timer`. Other user jobs are not changed.

## Launch and recovery

The drain asks Capstan what this machine may take. The claim response supplies the authoritative
spoke id. Local configuration and request fields cannot replace it.

A new leaf or cold orchestrator launch follows this sequence:

1. Spawn the configured Solo agent tool with a fresh UUIDv7 launch nonce.
2. Wait for the harness prompt. If Solo shows the folder-trust screen, send Down and Enter as separate
   inputs, then wait for the prompt again.
3. Record `delivered` and `routed` with Capstan.
4. Type a pointer of at most 299 bytes naming the signed brief link and nonce. Send Enter separately.
5. Read rendered output and require the pointer echo.

The full brief, bearer, and signed links never enter argv, process names, the global state file, or
errors. Signed links stay in the item directory's private `links.json`. The session reference contains
only `{spoke_id, solo_project_id, solo_process_id, launch_nonce, harness, agent_tool_id}`.

An orchestrator session is reusable only when the server returns its receipt-bound reference, the
reference nonce matches the current claim, Solo reports the exact process alive, and Solo reports it
idle. Idle does not mean finished. Leaf work never reuses a seat.

Capstan chooses repair actions. The drain has no repair timer or local retry ladder:

- `send_enter` reads the rendered prompt and submits only the pending pointer.
- `resend_pointer` sends Esc, types the current short pointer, sends Enter separately, and checks its
  echo.
- `respawn` closes the exact old Solo process and starts the mapped tool with the nonce in the
  directive.

If the Solo PTY rejects the same input twice, the launcher closes that exact process and reports the
observed action failure. A later drain tick follows the server's claim and directive response.

## Setup self-test

Setup starts a loopback Work stub with one harmless orchestrator item. It uses the saved Solo project,
agent tool, harness, MCP path, and launch sequence, while keeping the stub away from project files and
the configured Capstan server. The stub records five rungs: claim, spawn, nonce-bearing brief fetch,
milestone, and done. Each observed rung is printed. Missing evidence or a timeout fails setup; no rung
is inferred from Solo idle state.

The command runs one final drain tick after `done`, checks that local state dropped the completed claim,
and removes its private temporary directory. The same launcher and session identity then support a later
live AC10 run against Capstan; passing the loopback self-test is not reported as live AC10 evidence.
