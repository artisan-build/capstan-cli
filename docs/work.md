# Capstan Work setup

`capstan work drain` is a one-shot command. Solo runs local agents, and optional Amp leaf roles run
in remote orbs. Launchd or systemd starts a fresh drain tick each minute.

## Configure the machine

Interactive setup lists the Solo projects and agent tools installed on the machine before asking for
bindings:

```sh
capstan work setup
```

Each Solo role needs a numeric Solo project id, a numeric agent-tool id, and its harness (`claude`, `codex`,
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

### Amp leaf roles

Add repeatable `--amp-role builder --amp-role reviewer` flags to non-interactive setup, omit Solo
bindings for those roles, and keep the orchestrator's Solo binding. Setup discovers `amp` on PATH
and stores its absolute path; `--amp-path /absolute/path/to/amp` overrides discovery. It does not
read a token, change a role to Amp implicitly, or update Amp. The existing Amp installation owns
its authentication.

Setup writes version 3 only when Amp roles are explicit. All the version 2 fields remain, with these
changes (this fragment is not a complete config):

```json
{
  "version": 3,
  "launcher": "hybrid",
  "amp_path": "/absolute/path/to/amp",
  "amp_roles": ["builder", "reviewer"],
  "roles": ["builder", "reviewer", "orchestrator"],
  "role_providers": {"builder": "claude", "reviewer": "claude", "orchestrator": "opencode"},
  "solo_bindings": {
    "orchestrator": {"project_id": 12, "agent_tool_id": 34, "harness": "opencode", "extra_args": []}
  }
}
```

An Amp role cannot also have a Solo binding. Every remaining role needs its Solo binding, and
the orchestrator must stay local. Version 2 Solo configs and version 1 process configs remain supported.

## Schedule

`--schedule print` prints a complete launchd plist on macOS or a systemd user service and timer on
Linux. `--schedule install` writes only these dedicated files:

- macOS: `~/Library/LaunchAgents/com.artisan-build.capstan-work.plist`
- Linux: `~/.config/systemd/user/capstan-work.service` and `capstan-work.timer`

Install mode replaces anything at those dedicated paths with the generated schedule. It activates the named
launchd job or runs `systemctl --user enable --now capstan-work.timer`. Other user jobs are not changed.

## Launch and recovery

The drain asks Capstan what this machine may take. The claim response supplies the authoritative
spoke id. Local configuration and request fields cannot replace it.

A Solo leaf or cold orchestrator launch follows this sequence:

1. Spawn the configured Solo agent tool with the claim's server-issued UUIDv7 launch nonce.
2. Wait for the harness prompt. If Solo shows the folder-trust screen, send Down and Enter as separate
   inputs, then wait for the prompt again.
3. Record `delivered` and `routed` with Capstan.
4. Type a pointer of at most 900 bytes naming the exact signed brief link and nonce, using PTY
   writes of at most 299 bytes each. Send Enter separately after all chunks succeed.
5. Read rendered output and require the full pointer echo, allowing terminal wrapping.

The full brief, bearer, and signed links never enter argv, process names, the global state file, or
errors. Signed links stay in the item directory's private `links.json`. The session reference contains
only `{spoke_id, solo_project_id, solo_process_id, launch_nonce, harness, agent_tool_id}`.

An orchestrator session is reusable only when the server returns its receipt-bound reference, Solo
reports the exact referenced process alive, and Solo reports it idle. The reference keeps the nonce
that named the process when it was spawned. A warm wake uses the current item's separate nonce in
the signed pointer and `delivered` transition. Idle does not mean finished. Leaf work never reuses a
seat.

Capstan chooses repair actions. The drain has no repair timer or local retry ladder:

- `send_enter` reads the rendered prompt and submits only the pending pointer.
- `resend_pointer` sends Esc, types the current short pointer, sends Enter separately, and checks its
  echo.
- `respawn` closes the exact old Solo process and starts the mapped tool with the nonce in the
  directive.
- `nudge` sends bounded text and a separate Enter to the exact identity-checked Solo process.
  Generic process launchers report this action as unsupported.

If the Solo PTY rejects the same input twice, the launcher closes that exact process and reports the
observed action failure. A later drain tick follows the server's claim and directive response.

### Remote dispatch and verification

Every claim and renew negotiates `item_shape: "repository-v1"`. Amp work must name the task's
explicit lowercase GitHub `repository`, such as `example/application`. Null or invalid repository
values release the Amp claim with a redacted reason. Solo work does not require this field.
The `workflow_ref` identifies the workflow repository; it may differ from the target code repository
and never supplies an Amp project. A repository change invalidates that Amp binding: the drain
releases the affected item under its current fence and forgets it after success or a conflict response.
If release fails, it retains that holding for retry. It never kills or archives the old thread.

The configured executable must report Amp CLI `0.0.1792929650` or newer. Commands use direct argv,
with no shell or automatic update. The drain gets the exact project with
`amp projects get owner/repo --json`. Only an explicit not-found result permits
`amp projects create https://github.com/owner/repo --personal --json`. It then re-gets the project,
including after a competing create, to validate its id, Amp URL, and repository association.

Dispatch uses:

```text
amp -ox <short MCP pointer> --project owner/repo --mode claude-code --title <nonsecret title> --stream-json --no-archive-after-execute
```

The pointer names Capstan MCP `work_brief`, the item, and its launch nonce. It asks the orb to retry
briefly while the new binding becomes visible and contains no signed URL or bearer. The first
bounded event must be `system/init` with a valid `T-…` session id. Its canonical `amp:v1:` reference
contains exactly `project_id`, `thread_id`, and `thread_url`. The delivered transition records that
identity with the current nonce before the routed transition.

A private detached helper drains the remaining stream. Initialization waits at most one minute;
the helper bounds its turn to 24 hours and caps stdout at 1 MiB and stderr at 64 KiB. Other Amp
commands have a two-minute timeout and 64 KiB output limits. A saved project binding and init event let recovery reuse
the dispatch. If a start marker exists without an init event, recovery retains the item, nonce,
marker, and empty session reference without dispatching again under that nonce. It logs only the
item id and fence, continues sibling recovery, and includes both holdings in the Claim heartbeat.
It does not invent receipt metadata or filter repair directives for that ambiguous holding, so a
server-authorized `respawn` with a rotated nonce can reach the existing repair path. Each tick still
re-waits the one-minute initialization bound for every ambiguous item before moving on. This delay
can accumulate across ambiguous holdings. The CLI never archives or deletes a thread as cleanup.

Remote liveness comes from Capstan. A local streaming process can exit while the orb remains alive.
`awaiting_verification` is held and renewed without another launch or routing action. When the Solo
orchestrator accepts the finish, renewal eventually returns `lost` and local state is forgotten.
The thread is left alone. The drain never calls `gh`, checks a PR, or needs a GitHub credential.

Corrections move the same item from `awaiting_verification` back to `routed`. On every renewed
`routed` Amp holding, the drain fetches the refreshed signed `steers` URL and continues the existing
thread with each unseen item-scoped steer. Ordinary orchestrator item steers arrive through this
same path. A finish/corrections episode can happen entirely between drain ticks; delivery does not
depend on observing the intermediate waiting state:

```text
amp threads continue T-… -ox <bounded message>
```

Steer ids and pending-delivery state survive a crash; steer text stays out of recovery metadata,
logs, and thread titles. Later renewals still poll but do not resend delivered steers. A crash between
remote delivery and the local acknowledgment may retry the text. The delivery ledger holds at most
256 delivered steer ids per claim and fails closed when full. Pending ids are saved before delivery.
If a known pending steer is absent from the response, that item's delivery remains pending.
An empty poll with no pending ids succeeds without sending anything. Polling steers does not report
progress or reset the server's work deadline.

Steer text is limited to 8,000 Unicode characters and at most 32,000 UTF-8 bytes. The signed response
has its own 25,628,672-byte cap, sized for 256 entries with worst-case JSON-escaped Unicode and bounded
metadata. Amp command output and stream events keep their separate 64 KiB cap. More than 256 returned
entries fails that item closed; this client does not page server steers.

A steer fetch, validation, or delivery failure skips that item for the tick and logs only its id and
fence. Pending ids survive for retry; the drain still recovers siblings, sends the claim heartbeat,
and handles their directives. It also skips directives for an item whose delivery or repository
binding failed that tick. A local persistence failure can still stop the tick.

Polling marks all returned steers read, including task-wide entries that the drain does not deliver.
The orb can still read recent entries within the server's retention window. A known pending id that
ages out requires operator attention; it is never silently acknowledged.

The server's deadline `nudge` uses the same continuation command. Amp `send_enter` and
`resend_pointer` repairs continue that exact thread with an MCP pointer. Only an authorized `respawn`
starts a fresh thread under the server's rotated nonce; its outcome reports the replacement reference.
The drain has no local continuation timer or GitHub acceptance decision.

## Setup self-test

Setup starts a loopback Work stub with one harmless orchestrator item. It uses the saved Solo project,
agent tool, harness, MCP path, and launch sequence, while keeping the stub away from project files and
the configured Capstan server. The stub records five rungs: claim, spawn, nonce-bearing brief fetch,
milestone, and done. Each observed rung is printed. Missing evidence or a timeout fails setup; no rung
is inferred from Solo idle state.

The command retains the exact nonce-bound session before its final lost/forget tick. It checks that
local state dropped the completed claim, closes only that exact Solo process on success or failure,
and removes its private temporary directory. This self-test exercises Solo even in hybrid mode;
it makes no Amp request. The same launcher and session identity then support a later
live AC10 run against Capstan; passing the loopback self-test is not reported as live AC10 evidence.

## Pointer delivery decision

| Date | Decision | Reason |
| --- | --- | --- |
| 2026-10-25 | Share Continue’s chunked writer for gate and resend pointers; cap each write at 299 bytes and the full pointer at 900 bytes. | Signed URLs vary by installation domain; preserve the full URL and nonce while staying below the observed roughly 1000-byte message clipping. Enter remains separate, and two rejected sends still close the exact process. |
