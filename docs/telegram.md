# Using Hive from Telegram

Telegram is a Hive transport. It uses **long polling** (`getUpdates`), so Hive
needs no public endpoint, no inbound tunnel, and no webhook URL.

```text
Telegram  <--getUpdates (long poll)--  hive-plugin-telegram  --transport.inbound-->  Hive
                                             ^                                    |
                                             |                                    |
                                             +------------- events ---------------+
```

The transport owns Telegram syntax. Hive owns the operation semantics. A command
is never silently forwarded to the agent as a prompt, and ordinary text is never
treated as a command.

Telegram is richer than Zalo: it has structured mentions and commands, inline
buttons, message editing, and file uploads, so a permission request is answered
with a button and a picture an agent sends can be a local file.

---

## 1. Create the bot and get a token

1. Talk to **@BotFather** and run `/newbot`. Copy the token it answers with.
   It looks like `<bot id>:<secret>`.
2. That token is the only credential the transport needs.

## 2. Configure Hive

Credentials come from the environment, so they never appear in a config file:

```sh
export TELEGRAM_BOT_TOKEN='123456789:ABC...'
```

`TELEGRAM_TOKEN` is accepted as an alias, so a value kept in a `.env` file works
as it is. The variable must be in the environment of the `hive serve` process:
exporting it in another shell is not enough.

Then in `~/.hive/config.toml`:

```toml
[security]
# Deny by default. A Telegram user must be named here to talk to Hive.
allowed_users = ["telegram:123456789"]

[transport.telegram]
enabled = true

[transport.telegram.options]
bot_username = "hivebot"        # learned from getMe; this is the fallback
require_mention = "false"       # a private chat is always addressed
typing_indicator = "true"       # show Telegram's action and a ✍️ reaction on your message
```

Restart `hive serve`. You should see:

```text
level=INFO msg="plugin ready" plugin=telegram type=transport instance=telegram#1 capabilities=7
level=INFO msg="transport plugin ready" plugin=telegram
level=INFO msg="telegram bot connected" id=... username=hivebot
```

`telegram bot connected` is the line that matters. Without it, the token is
wrong or not in the environment.

If a webhook was previously registered for the bot, `getUpdates` refuses to
work and the log says so. Delete the webhook before using polling:

```sh
curl "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/deleteWebhook"
```

## 3. Who may talk to Hive

`allowed_users` is a list of **principals**, and a Telegram principal is
`telegram:<user id>`:

```toml
allowed_users = ["telegram:123456789"]
```

Your numeric user id is what bots like `@userinfobot` report.

**Unknown access is denied by default.** If the bot ignores a message, this
list is the first thing to check.

---

## Commands

Telegram intercepts a leading `/` and marks the token as a bot command, so a
command always arrives with its slash:

```text
/new_chat            works
new_chat             a bare word is a prompt, not a command
```

In a group a command can name its bot: `/cancel@hivebot` is the `cancel`
command. The command must be the **first token**.

The catalog below is the transport's own, generated from the map in
`plugins/telegram/parse.go`.

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
| `start` | | Alias for `help`; the message Telegram opens a bot chat with. |

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
```

### Images and files

Send a picture, a document, or a voice note with your message and the agent
receives the bytes, not a file name. The transport fetches the file with
`getFile`, because reading the platform is its job, and the core carries only
the bytes. A picture arrives as the largest rendition Telegram offers.

The size is bounded. A file larger than `max_attachment_mb` is skipped with a
warning instead of being read into memory. Telegram serves files up to 20 MB.

```toml
[transport.telegram.options]
max_attachment_mb = "8"     # default
```

An answer that embeds a markdown image — `![alt](target)` — is sent as a
photo, because Telegram draws no picture inside a text message. An http(s)
target is a URL the platform fetches itself; a local path is **uploaded**,
which Telegram accepts and Zalo does not. A photo that fails to send is posted
as a text reference rather than silently dropped. The image's alt text becomes
the photo's caption.

### Groups

A private chat is 1:1, so every message is addressed to the bot. A group is
shared, so with `require_mention = "true"` a message must address the bot —
Telegram parses the addressing itself:

```text
@hivebot what is this?          addressed: a mention
/cancel@hivebot                 addressed: a command
(a reply to the bot's message)  addressed: a reply
what is this?                   ignored in a group
```

A forum supergroup binds **per topic**: each topic is its own conversation
with its own session, and answers land in the topic that asked. Make one by
enabling **Topics** in the group's settings, then create a topic per project
or agent and address the bot inside it.

A bot joins a group with **privacy mode** on — that is Telegram's default,
not Hive's — and it then receives only:

- commands addressed to it (`/cancel@hivebot`),
- replies to its own messages,
- service messages (joins, topic creation, ...).

A plain `@hivebot hello` is **never delivered** — the log shows nothing at
all. To hear everything, make the bot a group **administrator** (group
settings → Administrators → Add Admin); bot admins always receive all
messages. The alternative is @BotFather → Group Privacy → Turn off, which
takes effect only after the bot is removed and re-added. `require_mention`
then decides which of the delivered messages matter.

### While a turn is working

Telegram has a transient **chat action**, so a turn shows it is working with
the `typing` indicator. Unlike a message, the action cannot be taken down: it
fades on its own after about five seconds. The transport refreshes it while
the turn runs and simply stops when the run finishes.

```toml
[transport.telegram.options]
typing_indicator = "true"     # default; shows Telegram's header "typing" action and
                            # marks your message with a ✍️ reaction while the turn
                            # runs. "false" says nothing until the answer
```

The transport shows the **answer** and **errors**, not the agent's steps. A
chat is not a log: a turn that narrated every tool call would bury the answer.
Use `/logs` when you want to see what happened.

### Conversations and sessions

A Telegram chat is bound to one Hive session the first time you talk to it.
Every later message in that chat reaches the same session, and the session
keeps its runs and its history.

- A chat with **no session yet** creates one. The first thing you say has to
  land somewhere.
- A chat that is **bound** is a prompt to that session.
- A finished run does not end the session. The next message continues it, with
  the same agent, as a new AgentRun.

### Continuing after a restart

An agent session lives in the agent process, so restarting Hive loses it. Two
things make that survivable:

- **The agent session is restored, not recreated.** When a run already had a
  runtime session and the agent can restore one, Hive asks it to.
- **What the transport missed is replayed.** A transport keeps a cursor for
  each conversation. On start it reads that cursor and renders what was
  published while it was down.

### Where the agent works

A session the transport creates names no workspace, so its runs work in
`workspace_dir` from the configuration — the default is `~/.hive/workspace`.
See [slack.md](slack.md) for the same rules, including the macOS permission
dialog.

### Redelivery and repeats

Telegram redelivers an update until it is acknowledged, and the offset is the
acknowledgement. Hive additionally derives the command id from the update id,
so a redelivery resolves to the same operation instead of running it twice.

---

## Permission requests

An agent may ask before running a tool. Hive relays the request to the
conversation, and **Telegram has buttons**, so the request renders its options
as an inline keyboard:

```text
🔐 Permission requested
Write file: /Users/you/project/internal/control/service.go

1. Allow once
2. Allow always
3. Deny

[Allow once]  [Allow always]  [Deny]   ← tap a button
```

Tap a button, or reply with the number of a choice or `allow` / `deny`. A
reply is only read as a decision while a request is pending; the rest of the
time "allow" is ordinary text. Once a press resolves, the keyboard comes down
so it cannot answer twice.

A permission request **fails closed**:

- It never becomes an implicit approval, including if the connection is lost.
- Once answered it cannot change, and a second reply observes the resolved
  state rather than authorising twice.
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
[transport.telegram]
acknowledgement.enabled = true
```

It means **"Hive received this"** and nothing more. The signal is the `typing`
action. Its failure never fails the operation.

---

## Troubleshooting

**Nothing happens at all.**
Look for `telegram bot connected` in the log. If it is absent,
`TELEGRAM_BOT_TOKEN` is missing or wrong, or not in the environment of the
`hive serve` process.

**`telegram getUpdates failed` and the description mentions a webhook.**
A webhook is registered for the bot, and Telegram does not allow `getUpdates`
while one is set. Delete the webhook (see above).

**The bot ignores my messages.**
1. Is your user id in `security.allowed_users`? Deny by default.
2. In a group, can the bot read the message at all? With privacy mode on —
   the default — only commands addressed to it and replies to its own
   messages are delivered; an @mention never arrives. Make the bot an
   administrator (see *Groups*).
3. In a group the bot can read, does the message address it — a mention, a
   reply, or a command — when `require_mention` is on?

**A reply never appears.**
Check the log for `could not post to Telegram`. Telegram refuses a text over
4,096 characters, which is why a long answer is posted as several messages.

**A turn never finishes.**
The agent is probably waiting for permission. See *Permission requests*.

---

## What the transport does not do

- It does not decide Hive semantics. `/new_chat` is a Telegram name; the
  operation is `session.create`.
- It does not reach session internals. It normalizes Telegram input into an
  envelope and hands it to Hive.
- It does not hold agent credentials. Those belong to the agent, not the
  transport.
- It does not use a webhook. Polling needs no public endpoint, which is what
  makes it workable on a personal installation.
- It does not read edited messages or channel posts. Only `message` and
  `callback_query` updates are delivered.
