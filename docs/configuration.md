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
