# Configuration reference

Hive reads one TOML file, `~/.hive/config.toml` by default. Unknown keys are
rejected, so a typo fails loudly instead of being ignored.

```toml
data_dir = ""          # default: ~/.hive/data
default_agent = ""     # default: the only agent, or the first by name

[cluster]
role = "auto"          # auto | coordinator | node
node_id = ""           # default: the hostname
listen = ""            # coordinator node link; default: 127.0.0.1 on a free port
coordinator_url = ""   # required when role is node
spawn_node = true      # a coordinator starts a node child process
lease_seconds = 0      # default: 30

[log]
level = "info"         # debug | info | warn | error
format = "text"        # text | json

[event_store]
retention_days = 30

[security]
allowed_users = []     # user principals a transport may assert
allowed_channels = []

[agents.<name>]
protocol = "acp"
command = ["claude", "acp"]   # or endpoint = "wss://..." for a remote agent

[transport.<name>]
enabled = false
[transport.<name>.options]    # opaque key/value pairs passed to the plugin

[transport.<name>.acknowledgement]
enabled = false
mode = "reaction"      # reaction | visual | none
reaction = "eyes"      # transport-specific presentation data
```

## Roles

`role = "auto"` resolves from the configuration: with a `coordinator_url` this
process is a node, without one it is the coordinator. A single-machine
installation therefore needs no role at all.

A coordinator starts a node child process by default, which is what makes one
`hive serve` bring up the whole stack. Set `spawn_node = false` to run the planes
separately.

## Agents

Hive core holds no vendor knowledge. An agent is a protocol plus a command, and
every ACP agent is served by the same adapter:

```toml
[agents.claude]
protocol = "acp"
command = ["claude", "acp"]

[agents.devin]
protocol = "acp"
command = ["devin", "acp"]
```

The agent name is also the plugin identity, so a node declares which agents it
runs and the coordinator routes execution work by that name.

Remote agents (`endpoint`) are recognized by the configuration and not
implemented in v1; starting with one fails rather than silently doing nothing.

## Transports

Transport options are opaque to the core: it passes them through, so Hive holds
no vendor-specific configuration fields. The Slack transport reads:

```toml
[transport.slack]
enabled = true
[transport.slack.options]
bot_user_id = "U0XXXXXXX"      # the bot's own user id, for mention resolution
require_mention = "true"       # ignore messages that do not address the bot
```

Credentials come from the environment so they never appear in configuration:

```sh
export SLACK_APP_TOKEN=xapp-...   # Socket Mode connection
export SLACK_BOT_TOKEN=xoxb-...   # Web API calls
```

## Plugin binaries

Plugin binaries ship next to the `hive` binary. `HIVE_PLUGIN_DIR` overrides the
lookup directory, which is what makes `go run` and the tests workable.

## Storage

The coordinator database is `~/.hive/data/coordinator.db` and the node database
is `~/.hive/data/node.db`. The node database is local execution state and an
event buffer, not a second authoritative copy of anything.

`event_store.retention_days` prunes durable events that have already been
published. Event retention and session retention are independent: a session can
outlive its old events.

## Control API

The coordinator serves the Control API on an owner-only unix socket at
`~/.hive/data/hive.sock`. The operating system authenticates the caller, so the
local CLI and TUI need no credential. A unix socket path is bounded by the
platform's `sockaddr_un`, so a very long `data_dir` is rejected with a message
that says so.


## Running as a background service

```sh
hive service install     # start at login, restart if it stops
hive service status      # is it running
hive service uninstall   # stop and remove
```

`install` writes a launchd agent on macOS and a systemd user unit on Linux. It
also does three things that a naive service definition gets wrong:

**It copies the binaries next to the data directory.** On macOS a background
agent cannot execute a binary under a protected directory such as `Documents`:
the attempt is blocked without a visible prompt, which looks exactly like a
daemon that starts and does nothing. A service also should not depend on a
checkout that can move. The daemon and its plugins are copied to
`~/.hive/data/bin/`.

**It captures `PATH`.** A background service does not inherit a login shell's
environment, and the agents live on your `PATH`. Without this the daemon starts,
listens, and cannot run any agent.

**It keeps secrets out of the service definition.** The variables the
configuration references are written to `~/.hive/data/service.env`, readable by
its owner only. On macOS the service definition can be printed with `launchctl
print`, so a token stored there is visible to anything that can talk to the
supervisor.

Only what the configuration references is captured, so installing the service
does not quietly copy every secret in your shell into a file.

```sh
hive service install
# hive: the service will inherit PATH from this shell
# hive: the daemon will start at login
#   binary: /Users/you/.hive/data/bin/hive
#   secrets: /Users/you/.hive/data/service.env (0600)
```

## Measuring what it costs

```sh
scripts/measure.sh.py        # set HIVE_PID to the daemon pid
```

It samples the process tree rooted at the daemon and counts only Hive's own
processes: an agent runtime you installed is your cost, not Hive's, and counting
it would drown the number that matters.

Measured on a five-process tree (daemon, node child, Slack transport, two agent
bridges) while running agent turns:

| Process | Memory |
|---|---|
| `hive serve` | 27 MB |
| `hive` (node child) | 24 MB |
| `hive-plugin-slack` | 22 MB |
| `hive-plugin-acp` (devin) | 11 MB |
| `hive-plugin-acp` (hermes) | 8 MB |
| **total** | **~92 MB** |

CPU is about 0% when idle and peaks near 2% during a turn.


## Agent authentication

An agent may advertise auth methods, and Hive does not send one preemptively.

That is deliberate, and it is the difference between one browser window and one
per agent launch. Credentials are the agent's to keep: an agent that already has
them is refused nothing, and sending the method anyway runs its whole flow again.
An agent whose method opens a browser opened one every time it was started, even
though the credentials it had just saved were sitting on disk.

The protocol lets an agent refuse a session when it needs authentication, so Hive
waits to be told:

- An agent that has credentials is never sent through the flow.
- An agent that does not gets authenticated once, and the session is retried.

```toml
[agents.devin]
protocol = "acp"
command = ["devin", "acp"]
auth_method = ""            # empty uses the first method the agent offers
api_key_env = "DEVIN_API_KEY"   # for a method that authenticates with a key
```

A method that needs a browser is yours to complete. When one is tried and fails,
the error names the method, so it is clear which flow the agent wanted.

## What an agent launch costs

An agent process lives as long as the plugin that started it, and it ends with
it. A plugin that exits without ending its agent leaves it running: it holds a
session, its memory, and whatever authentication state it has, and nothing will
ever collect it.
