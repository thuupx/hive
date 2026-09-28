# Using Hive from Zalo

Zalo is a Hive transport. It uses **long polling** (`getUpdates`), so Hive needs
no public endpoint, no inbound tunnel, and no webhook URL.

```text
Zalo  <--getUpdates (long poll)--  hive-plugin-zalo  --transport.inbound-->  Hive
                                          ^                                    |
                                          |                                    |
                                          +------------- events --------------+
```

The transport owns Zalo syntax. Hive owns the operation semantics. A command is
never silently forwarded to the agent as a prompt, and ordinary text is never
treated as a command.

Zalo is simpler than Slack: there is no socket mode, no message editing, no
reactions, and no threads. The transport is built around what the platform can
actually do.

---

## 1. Create the bot and get a token

1. Create a Zalo Bot and copy its **Bot Token**. It looks like
   `<bot id>:<secret>`.
2. That token is the only credential the transport needs.

## 2. Configure Hive

Credentials come from the environment, so they never appear in a config file:

```sh
export ZALO_BOT_TOKEN='1681425009216512167:...'
```

`ZALO_TOKEN` is accepted as an alias, so a value kept in a `.env` file works as
it is. The variable must be in the environment of the `hive serve` process:
exporting it in another shell is not enough.

Then in `~/.hive/config.toml`:

```toml
[security]
# Deny by default. A Zalo user must be named here to talk to Hive.
allowed_users = ["zalo:6ede9afa66b88fe6d6a9"]

[transport.zalo]
enabled = true

[transport.zalo.options]
bot_name = "Bot Hive Agent"     # from getMe; used to recognize @mentions in a group
require_mention = "false"       # a private chat is always addressed
typing_indicator = "true"       # show Zalo's "typing" action while a turn runs
```

Restart `hive serve`. You should see:

```text
level=INFO msg="plugin ready" plugin=zalo type=transport instance=zalo#1 capabilities=7
level=INFO msg="transport plugin ready" plugin=zalo
level=INFO msg="zalo bot connected" id=... name="Bot Hive Agent"
```

`zalo bot connected` is the line that matters. Without it, the token is wrong or
not in the environment.

If a webhook was previously registered for the bot, `getUpdates` refuses to work
and the log says so. Delete the webhook before using polling.

## 3. Who may talk to Hive

`allowed_users` is a list of **principals**, and a Zalo principal is
`zalo:<user id>`:

```toml
allowed_users = ["zalo:6ede9afa66b88fe6d6a9"]
```

**Unknown access is denied by default.** If the bot ignores a message, this list
is the first thing to check.

---

## Commands

Zalo does not intercept a leading `/`, so both forms reach the bot:

```text
/new_chat            works
new_chat             works: the slash is optional
```

The command must be the **first token**:

```text
/agents              the command is first, so it is a command
please run /agents   the command is not first, so this is a prompt
```

The catalog below is the transport's own, generated from the map in
`plugins/zalo/parse.go`.

| Command | Arguments | What it does |
|---|---|---|
| `new_chat` | `[agent]` | Create a session and its first AgentRun. Optional argument selects the agent. |
| `new` | `[agent]` | Alias for `new_chat`. |
| `agents` | | List the agents this installation can run. |
| `nodes` | | List the nodes the coordinator has heard from. |
| `status` | | Show the current session: state, runs, agent, node. |
| `cancel` | | Cancel the run that is currently working. |
| `handoff` | `<agent>` | Hand the session to another agent, carrying the context. |
| `model` | `[name]` | Show the agent's models, or switch to one. |
| `models` | | Alias for `model` with no argument. |
| `mode` | `[name]` | Show the agent's session modes, or switch to one. |
| `sessions` | | List the conversations you can continue. |
| `logs` | `[from] [limit]` | Show recent activity for this conversation. |
| `help` | | Show this list. |

Anything else is a **prompt**.

### Examples

```text
/new_chat
  → New chat created.
    Session: sess_a9dd4304ebe98911
    Agent: hermes

fix the failing test in internal/control
  → Working on it.
  → (the answer arrives as a message when the turn ends)

/status
  → Session sess_a9dd4304ebe98911
    State: active
    • run_baf592166953a486 hermes on your-host — running

/agents
  → Agents
    • devin (acp)
    • hermes (acp)
```

### Images

Send a picture with your message and the agent receives it as a picture, not as a
file name. The transport fetches the bytes, because reading the platform is its
job, and the core carries only the bytes.

A picture with no caption still reaches the agent. A sticker or a voice note is
not something an agent can act on, so it is ignored rather than guessed at.

The size is bounded. A file larger than `max_attachment_mb` is skipped with a
warning instead of being read into memory.

```toml
[transport.zalo.options]
max_attachment_mb = "8"     # default
```

An answer that embeds a markdown image — `![alt](target)` — arrives as a photo,
because Zalo draws no picture inside a text message. The target may be an
http(s) URL, which the platform fetches itself, or a file path on the machine,
which the transport reads and uploads. `max_attachment_mb` bounds the read in
that direction too, and the image's alt text becomes the photo's caption. A
picture that cannot be sent is reported in words rather than dropped.

### Groups

A private chat is 1:1, so every message is addressed to the bot. A group is
shared, so with `require_mention = "true"` the message must open with a command
or mention the bot by name:

```text
@Bot Hive Agent what is this?     addressed
/status                            addressed: a command
what is this?                      ignored in a group
```

Group support is **Beta** on Zalo.

### While a turn is working

Zalo has a transient **chat action**, so a turn shows it is working with the
`typing` indicator. Unlike a message, the action cannot be taken down: it fades on
its own. The transport refreshes it while the turn runs and simply stops when the
turn ends.

```toml
[transport.zalo.options]
typing_indicator = "true"     # default; "false" says nothing until the answer
```

The transport shows the **answer** and **errors**, not the agent's steps. A chat
is not a log: a turn that narrated every tool call would bury the answer. Use
`/logs` when you want to see what happened.

### Conversations and sessions

A Zalo chat is bound to one Hive session the first time you talk to it. Every
later message in that chat reaches the same session, and the session keeps its
runs and its history.

- A chat with **no session yet** creates one. The first thing you say has to land
  somewhere.
- A chat that is **bound** is a prompt to that session.
- A finished run does not end the session. The next message continues it, with
  the same agent, as a new AgentRun.

### Continuing after a restart

An agent session lives in the agent process, so restarting Hive loses it. Two
things make that survivable:

- **The agent session is restored, not recreated.** When a run already had a
  runtime session and the agent can restore one, Hive asks it to.
- **What the transport missed is replayed.** A transport keeps a cursor for each
  conversation. On start it reads that cursor and renders what was published while
  it was down.

### Where the agent works

A session the transport creates names no workspace, so its runs work in
`workspace_dir` from the configuration — the default is `~/.hive/workspace`. See
[slack.md](slack.md) for the same rules, including the macOS permission dialog.

### Redelivery and repeats

Zalo may redeliver an update. Hive derives the command id from the message id, so
a redelivery resolves to the same operation instead of running it twice.

---

## Permission requests

An agent may ask before running a tool. Hive relays the request to the
conversation. **Zalo has no buttons**, so the card says what to type:

```text
🔐 Permission requested
Write file: /Users/you/project/internal/control/service.go

Reply with the number:
1. Allow once
2. Allow always
3. Deny

Or reply `allow` / `deny`.
```

Reply with the number of a choice, or with `allow` / `deny`. A reply is only read
as a decision while a request is pending; the rest of the time "allow" is
ordinary text.

A permission request **fails closed**:

- It never becomes an implicit approval, including if the connection is lost.
- Once answered it cannot change, and a second reply observes the resolved state
  rather than authorising twice.
- If nobody answers, it expires into a safe terminal state.

With no transport connected, the CLI is the fallback:

```sh
hive permission list
hive permission respond <agent-request-id> -allow
```

---

## Acknowledgement

Optional, and on by default.

```toml
[transport.zalo]
acknowledgement.enabled = true
```

It means **"Hive received this"** and nothing more. Zalo has no reactions, so the
signal is the `typing` action. Its failure never fails the operation.

---

## Troubleshooting

**Nothing happens at all.**
Look for `zalo bot connected` in the log. If it is absent, `ZALO_BOT_TOKEN` is
missing or wrong, or not in the environment of the `hive serve` process.

**`zalo getUpdates failed` and the description mentions a webhook.**
A webhook is registered for the bot, and Zalo does not allow `getUpdates` while
one is set. Delete the webhook.

**The bot ignores my messages.**
1. Is your user id in `security.allowed_users`? Deny by default.
2. In a group, does the message address the bot, or open with a command?

**A reply never appears.**
Check the log for `could not post to Zalo`. Zalo refuses a text over 2,000
characters, which is why a long answer is posted as several messages.

**A turn never finishes.**
The agent is probably waiting for permission. See *Permission requests*.

---

## What the transport does not do

- It does not decide Hive semantics. `/new_chat` is a Zalo name; the operation is
  `session.create`.
- It does not reach session internals. It normalizes Zalo input into an envelope
  and hands it to Hive.
- It does not hold agent credentials. Those belong to the agent, not the
  transport.
- It does not use a webhook. Polling needs no public endpoint, which is what makes
  it workable on a personal installation.
