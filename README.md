# Hive

A self-hosted personal agent gateway.

Hive connects clients and platforms to agent runtimes across machines. It owns
orchestration, identity, routing, context, events, handoff, and node management.
Agents own reasoning, tools, and their own filesystem and shell semantics.
Transports own presentation.

```text
client / transport
        |
        v
   Hive coordinator  ---- node link ---- node
        |                                  |
   sessions, runs,                    agent plugin
   events, commands,                        |
   handoff, permissions               agent process (ACP)
```

One `hive serve` brings up the coordinator, a node child process, and the
configured agent plugins.

---

## Requirements

| | |
|---|---|
| Go | 1.27 or newer, with CGO enabled |
| C toolchain | clang or gcc; storage uses libSQL through CGO |
| An agent | a process that speaks [ACP](https://agentclientprotocol.com) over stdio |

**Platforms:** linux and darwin, on amd64 and arm64. Windows is not supported,
because the libSQL driver ships prebuilt native libraries for those four
combinations only. See `docs/adr/0001-storage-libsql.md`.

## Build

```sh
git clone <this repo> && cd Hive
make build          # bin/hive, bin/hive-plugin-acp, bin/hive-plugin-slack
make examples       # bin/example-client, bin/plugin-echo
make test           # the whole suite
make ci             # vet, test, build
```

`make build` writes every binary into `bin/`. They ship next to each other,
because the daemon resolves plugins from its own directory.

---

## Quick start

### 1. Create a configuration

```sh
./bin/hive init
```

`hive init` looks for ACP agents on your `PATH`, and writes
`~/.hive/config.toml` naming what it found:

```text
Found 2 ACP agent(s) on PATH:

  devin            devin acp  (ACP invocation is a convention: verify it)
  hermes           hermes-acp

Which agent should be the default for new sessions?

  1) devin
  2) hermes

Choose 1-2, or a name (default: devin):
```

It writes **every key** with its effective default, so nothing has to be guessed
and nothing depends on knowing what a zero value means. It refuses to overwrite
an existing file unless you pass `-force`.

How discovery works, and what it cannot know:

- A dedicated **ACP adapter** is named for what it is (`<something>-acp`), so a
  `PATH` scan finds adapters Hive has never heard of without guessing an
  invocation.
- A **tool that exposes ACP as a mode** is launched with that subcommand. That is
  a convention, not a fact, so `hive init` marks it: *verify it against this
  tool's documentation*.
- Nothing found? The file says so, and shows a commented example to fill in.

Flags for scripting:

```sh
hive init -agent hermes      # choose without prompting
hive init -force             # overwrite an existing file
```

### 2. Check the agent section

Hive does not ship an agent. It speaks ACP to whatever you have installed. The
generated file should contain something like:

```toml
default_agent = "hermes"

[agents.hermes]
protocol = "acp"
command = ["hermes-acp"]
endpoint = ""
```

The `command` is the program Hive launches and talks ACP to. The agent name is
also the plugin identity, so it is what you pass to `-agent` and what a node
declares it can run.

A run also needs a working directory: agents write files, and ACP requires a
`cwd` to create a session. `workspace_dir` sets it. Empty means the directory the
node was started in, which is usually what you want.

> **This is the step people miss.** Without an `[agents.*]` section, Hive starts
> and serves status, but it cannot create a session, and it will not start a node
> child process. `hive agent list` prints `no agents configured`.

### 3. Run the daemon

```sh
./bin/hive serve
```

Leave it running. You should see:

```text
level=INFO msg="coordinator listening" url=wss://127.0.0.1:52145/node
level=INFO msg="control api listening" socket=/Users/you/.hive/data/hive.sock
level=INFO msg="coordinator ready" url=... agents=1 control=...
level=INFO msg="node child started" pid=2685
level=INFO msg="plugin ready" plugin=claude type=agent instance=claude#1
level=INFO msg="node connected" node=your-host version=0.1.0-dev generation=1
```

### 4. Use it from another terminal

The CLI talks to the running daemon over an owner-only unix socket.

```sh
./bin/hive agent list     # what agents this installation can run
./bin/hive node list      # which nodes are connected
./bin/hive session create # start a session and its first AgentRun
```

```text
$ ./bin/hive session create
session: sess_b1d8d6dd8a860f4b
run:     run_305870203fb38fd5
agent:   claude
node:    your-host
command: cmd_b31f7baa139d552e
```

Then prompt it and read the answer:

```sh
./bin/hive session prompt sess_b1d8d6dd8a860f4b "explain this repository"
./bin/hive session status sess_b1d8d6dd8a860f4b
./bin/hive session events sess_b1d8d6dd8a860f4b
```

```text
$ ./bin/hive session events sess_b1d8d6dd8a860f4b
     1  agent.raw              run_15ae8808f22c5ec5
     ...
     8  message                This repository is a self-hosted agent gateway...
```

`agent.raw` is the protocol-native stream, preserved unmodified. `message` is the
one thing Hive normalizes out of it: the answer to your prompt, because otherwise
it would only be reachable as raw protocol chunks.

### 5. See the management plane

```sh
./bin/hive tui                 # render once
./bin/hive tui -interval 2s    # refresh every two seconds
```

---

## Command reference

Global flags work anywhere on the line, before or after the subcommand:

| Flag | Meaning |
|---|---|
| `-config <path>` | configuration file (default `~/.hive/config.toml`) |
| `-role <role>` | override `cluster.role`: `auto`, `coordinator`, `node` |
| `-coordinator-url <url>` | override `cluster.coordinator_url` |
| `-node-id <id>` | override `cluster.node_id` |

### Daemon

```sh
hive init [-agent <name>] [-force]   # write a starter configuration
hive config                    # validate and print the effective configuration
hive serve                     # run the daemon
hive version                   # build and protocol version
```

### Sessions

```sh
hive session create [-agent <id>] [-workspace <name>] [-command-id <id>]
hive session list [-limit <n>]
hive session status <session-id>
hive session prompt <session-id> <text...> [-run <run-id>]
hive session cancel <session-id> [-run <run-id>]
hive session handoff <session-id> <agent-id> [-summary <text>]
hive session events <session-id> [-from <sequence>] [-limit <n>] [-json]
```

### Everything else

```sh
hive agent list                # configured agents
hive node list                 # nodes the coordinator has heard from
hive command get <command-id>  # durable status of one operation
hive workspace create <name> [-node <id>] [-path <path>]
hive workspace list            # workspaces and shared-location warnings
hive tui [-interval <duration>]
```

### Why `-command-id` matters

Every mutating command carries an idempotency key. `hive session create` mints a
fresh one, because an invocation is a new logical operation. Passing
`-command-id` explicitly is what you do when **retrying** the same one:

```sh
hive session create -command-id my-retry-1
hive session create -command-id my-retry-1   # returns the same session, creates nothing
```

This is the difference between "try again" and "do it twice".

---

## Connecting Slack

Slack is the v1 production transport. It uses **Socket Mode**, so Hive needs no
public endpoint and no inbound tunnel.

### 1. Create the app

Create an app at <https://api.slack.com/apps> and enable **Socket Mode**. Slack
generates an app-level token (`xapp-...`) with the `connections:write` scope.

### 2. Scopes

Bot token scopes:

```text
app_mentions:read      see mentions of the bot
chat:write             post replies
reactions:write        the optional acknowledgement reaction
channels:history       read messages in public channels
groups:history         read messages in private channels
im:history             read direct messages
users:read             resolve user ids
```

### 3. Event subscriptions

Subscribe to `app_mention`, and to `message.channels` (plus `message.im` if you
want direct messages).

### 4. Install the app and invite it

Install to your workspace, then invite the bot to the channel you want to use:

```text
/invite @your-bot
```

### 5. Credentials

Credentials come from the environment so they never appear in a config file:

```sh
export SLACK_APP_TOKEN=xapp-1-...    # Socket Mode connection
export SLACK_BOT_TOKEN=xoxb-...      # Web API calls
```

### 6. Configure the transport

Find the bot's own user id (`auth.test` returns `user_id`), then:

```toml
[security]
allowed_users = ["slack:U123"]        # who may talk to Hive

[transport.slack]
enabled = true
[transport.slack.options]
bot_user_id = "U0XXXXXXX"             # the bot's own user id
require_mention = "true"              # ignore messages that do not address it
```

Restart `hive serve`. You should see `slack socket mode connected`.

### 7. Talk to it

```text
@your-bot /new_chat            create a session
@your-bot /agents              list agents
@your-bot /status              show the session
@your-bot /cancel              cancel the current run
@your-bot /handoff <agent>     hand the session to another agent
@your-bot fix the login bug    ordinary text becomes a prompt
```

A recognized command is never silently forwarded to the agent as a prompt, and
ordinary text is never treated as a command.

`allowed_users` is **deny by default**. If Slack messages do nothing, that list is
the first thing to check.

---

## How it works

Four ideas carry most of the design.

**A session is not an agent run.** A session is the durable conversation. An
AgentRun is one agent's execution inside it. Handing a session to another agent
creates a new AgentRun, and the two runtime session ids stay separate fields.
Hive never converts one runtime's native session into another's.

**Commands are idempotent, events are deduplicated.** A stable `command_id`
resolves a retry to the existing operation. A stable `event_id` means a
redelivered event is not stored twice and consumes no sequence.

**Sequence is ordering, not causality.** The event store assigns a
session-scoped monotonic sequence when an event becomes durable. A higher
sequence does not mean the later event observed the earlier one.

**A node owns execution, the coordinator owns state.** The node reports what
actually happened. A lease expiry classifies a run as interrupted and never
starts a replacement, because the original execution may still be alive.

More:

- `docs/protocol.md` — wire format, API domains, versioning, event types
- `docs/security.md` — identity classes, authorization, permissions
- `docs/configuration.md` — every configuration key
- `docs/failure-semantics.md` — what is guaranteed, what is best-effort, what is
  explicitly not claimed
- `hive-v1-implementation-plan.md` — milestones and what each one deliberately
  leaves out
- `AGENTS.md` — conventions for changing this codebase

---

## Troubleshooting

**`no agents configured`, and `node list` is empty.**
You have no `[agents.<name>]` section. Hive starts and serves status without one,
but it cannot create sessions and it will not start a node child process. Run
`hive init` and let it find your agents, or add a section by hand.

**`hive init` found an agent, but `hive serve` says the command is missing.**
The ACP invocation it wrote is a convention. Check that tool's documentation and
correct `command` in `~/.hive/config.toml`.

**`client: connect to ~/.hive/data/hive.sock: no such file or directory`.**
`hive serve` is not running, or it is running under a different `HOME` or
`data_dir`. Start it, or pass the same `-config` to the client.

**`agent could not be started: exec: "claude": executable file not found in $PATH`.**
The `command` in your `[agents.*]` section is not installed, or not on the `PATH`
of the daemon. The session and the run are still created and remain inspectable;
only the execution failed. Fix the command and create a new session.

**`zsh: no such file or directory: ./bin/hive`.**
You are not in the repository root. The binaries live in `bin/` after
`make build`.

**`config ...: unknown keys: agnet`.**
A typo. The message names the unrecognized top-level keys.

**`config ...: strict mode: fields in the document are missing in the target struct`.**
The key is nested inside a section rather than at the top level. Compare against
`docs/configuration.md`.

**`slack: an app token is required for Socket Mode`.**
`SLACK_APP_TOKEN` is not exported in the environment of the daemon process.

**A handoff failed and the session did not move.**
That is the designed behaviour: a handoff is only reported successful if the
target execution actually started. The failed attempt stays inspectable, the
target run is terminated, and the session keeps routing to its original run.

**`cursor expired: requested 0, next valid 5`.**
The events you asked for have been pruned by `event_store.retention_days`.
Rehydrate from the snapshot boundary the message names, then resume from the next
valid sequence.

---

## Development

```sh
make test              # go test ./...
make test-race         # the suite under the race detector
make vet
make fmt
make ci                # vet, test, build, examples
```

Tests are colocated with the code they test, which is the Go convention:
`package foo` for white-box tests, `package foo_test` for black-box ones.
Integration tests that need the whole stack live in `internal/daemon`.

The test suite re-executes its own binary as a plugin process, so the full
coordinator → node → plugin → agent path is exercised without a build step or a
fixture binary.

See `AGENTS.md` before changing anything: it records the conventions and the
mistakes that produced them.
