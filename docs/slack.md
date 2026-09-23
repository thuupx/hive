# Using Hive from Slack

Slack is the v1 production transport. It uses **Socket Mode**, so Hive needs no
public endpoint, no inbound tunnel, and no signing secret on the wire.

```text
Slack  --Socket Mode-->  hive-plugin-slack  --transport.inbound-->  Hive
                                    ^                                  |
                                    |                                  |
                                    +---------- events ---------------+
```

The transport owns Slack syntax. Hive owns the operation semantics. A command is
never silently forwarded to the agent as a prompt, and ordinary text is never
treated as a command.

---

## 1. Create the Slack app

1. Go to <https://api.slack.com/apps> and create an app from scratch.
2. Open **Socket Mode** and turn it on. Slack asks for a token with the
   `connections:write` scope and generates an **app-level token** (`xapp-...`).
   Keep it: this is `SLACK_APP_TOKEN`.
3. Open **OAuth & Permissions** and add these **bot token scopes**:

   | Scope | Why |
   |---|---|
   | `app_mentions:read` | see messages that mention the bot |
   | `chat:write` | post replies |
   | `reactions:write` | the optional acknowledgement reaction |
   | `channels:history` | read messages in public channels |
   | `groups:history` | read messages in private channels |
   | `im:history` | read direct messages |
   | `users:read` | resolve user ids |

4. Open **Event Subscriptions** and subscribe to bot events:

   | Event | Why |
   |---|---|
   | `app_mention` | a message that addresses the bot |
   | `message.channels` | ordinary messages in public channels |
   | `message.im` | direct messages |

   You do **not** need a request URL: Socket Mode delivers over the WebSocket.

5. **Install to Workspace** and copy the **Bot User OAuth Token** (`xoxb-...`).
   This is `SLACK_BOT_TOKEN`.
6. Invite the bot to the channel you want to use:

   ```text
   /invite @your-bot
   ```

## 2. Find the bot's own user id

The transport needs it to recognise a mention. It is not the bot id.

```sh
curl -s -X POST https://slack.com/api/auth.test \
  -H "Authorization: Bearer $SLACK_BOT_TOKEN" | python3 -m json.tool
```

```json
{ "ok": true, "user": "hive", "user_id": "U0C3L371Q8Z", "bot_id": "B0C32PHV2UX" }
```

Use `user_id`, the one that starts with `U`.

## 3. Configure Hive

Credentials come from the environment, so they never appear in a config file:

```sh
export SLACK_APP_TOKEN=xapp-1-...
export SLACK_BOT_TOKEN=xoxb-...
```

Then in `~/.hive/config.toml`:

```toml
[security]
# Deny by default. A Slack user must be named here to talk to Hive.
allowed_users = ["slack:U123ABC"]

[transport.slack]
enabled = true

[transport.slack.options]
bot_user_id = "U0C3L371Q8Z"    # the bot's own user id, from auth.test
require_mention = "true"        # ignore messages that do not address the bot

[transport.slack.acknowledgement]
enabled = false                 # optional; see below
reaction = "eyes"
```

Restart `hive serve`. You should see:

```text
level=INFO msg="plugin ready" plugin=slack type=transport instance=slack#1 capabilities=7
level=INFO msg="transport plugin ready" plugin=slack
level=INFO msg="slack socket mode connected"
```

`slack socket mode connected` is the line that matters. Without it, nothing
arrives.

## 4. Who may talk to Hive

`allowed_users` is a list of **principals**, and a Slack principal is
`slack:<user id>`:

```toml
allowed_users = ["slack:U123ABC", "slack:U456DEF"]
```

Find a user id in Slack: open the profile, **More**, **Copy member ID**.

**Unknown access is denied by default.** If the bot ignores a message, this list
is the first thing to check.

---

## Commands

### Slack reserves a leading `/`

Slack intercepts a message that **starts** with `/` and treats it as one of its
own slash commands. Such a message never reaches Hive at all — it is not a Hive
bug, and there is no way around it from inside the app.

So `/new_chat` on its own does nothing. Address the bot first:

```text
@your-bot /new_chat          works: the / is not at the start
@your-bot new_chat           works: the slash is optional
/new_chat                    intercepted by Slack, never reaches Hive
```

Both working forms are equivalent. The slash is presentation, and the transport
accepts either.

### The command catalog

The catalog below is the transport's own, generated from the map in
`plugins/slack/parse.go`.

The mention may appear anywhere in the message; it is removed before parsing:

```text
@your-bot /agents            the command is first, so it is a command
hey @your-bot /agents        the command is not first, so this is a prompt
```

Put the command first, right after the mention.

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

Anything else is **not** a command:

- Text that does not start with a known command name is a **prompt**.
- Text that merely contains a slash, such as `check src/main.go`, is a **prompt**.
- `/teleport` is recognised as a command form but exposes no operation, so it is
  reported as a command error rather than being sent to the agent.

### Examples

```text
@your-bot /new_chat
  → New chat created.
    Session: sess_a9dd4304ebe98911
    Agent: hermes

@your-bot fix the failing test in internal/control
  → Working on it.
  → (the answer arrives as a message when the turn ends)

@your-bot /status
  → Session sess_a9dd4304ebe98911
    State: active
    • run_baf592166953a486 hermes on your-host — running

@your-bot /handoff devin
  → Handed off. Session: sess_a9dd4304ebe98911
    Agent: devin
    Target run: run_74a213ace9aa1724

@your-bot /agents
  → Agents
    • devin (acp)
    • hermes (acp)

@your-bot /cancel
  → Cancelled.
```

### Agent settings

An agent declares what it lets you configure. Hive renders whatever it sent: it
does not know what a model is, only that the agent offers a selector.

```text
@your-bot /model
  → *devin settings*
    *Model* — now `swe-2-high`
    •  :white_check_mark: `swe-2-high` — SWE-2 High
    • `swe-2-fast` — SWE-2 Fast
    Switch with: `@Hive model <name>`

@your-bot /model swe-2-fast
  → the selector is applied to the live session

@your-bot /mode
  → *Session Mode* — now `accept-edits`
    • `accept-edits` — Code
    • `plan` — Plan
```

The choice is stored on the **session**, so a new AgentRun continues with it
instead of silently reverting. A value the agent refuses is reported rather than
silently dropped: a user who picked a model should not be left wondering which
one ran.

### Conversations and sessions

A Slack channel (or thread) is bound to one Hive session the first time you talk
to it. Every later message in that channel reaches the same session, and the
session keeps its runs and its history.

- A message in a channel with **no session yet** creates one. The first thing you
  say has to land somewhere.
- A message in a **bound** channel is a prompt to that session.
- A finished run does not end the session. The next message continues it, with
  the same agent, as a new AgentRun.

### Redelivery and repeats

Slack may redeliver an event. Hive derives the command id from the Slack event id,
so a redelivery resolves to the same operation instead of running it twice.

You repeating an action produces a new Slack event, so it is a new operation.
That is the difference between "Slack retried" and "I asked again".

---

## Permission buttons

An agent may ask before running a tool. Hive relays the request to the
conversation:

```text
Permission requested
Write file: /Users/you/project/internal/control/service.go

[ Allow ]  [ Deny ]
```

Press one. The decision is relayed to the agent, and the turn continues or stops.

A permission request **fails closed**:

- It never becomes an implicit approval, including if the connection is lost.
- Once answered it cannot change, and a second press observes the resolved state
  rather than authorising twice.
- If nobody answers, it expires into a safe terminal state.

If a turn appears to hang, check for a pending request. With no transport
connected, the CLI is the fallback:

```sh
hive permission list
hive permission respond <agent-request-id> -allow
```

---

## Acknowledgement

Optional, and off by default.

```toml
[transport.slack.acknowledgement]
enabled = true
reaction = "eyes"
```

It means **"Hive received this"** and nothing more. It is not "agent started",
"agent is working", or "agent finished". Its failure never fails the operation:
if the reaction cannot be added, the message is still processed.

---

## Troubleshooting

**Nothing happens at all.**
Look for `slack socket mode connected` in the log. If it is absent, the app token
is missing or wrong. `SLACK_APP_TOKEN` is an **app-level** token (`xapp-...`), not
the bot token.

**The bot ignores my messages.**
1. Is your user id in `security.allowed_users`? Deny by default.
2. Is the bot invited to the channel? `/invite @your-bot`.
3. With `require_mention = "true"`, the message must address the bot:
   `@your-bot ...`.
4. Is `bot_user_id` the bot's **user id** (`U...`), not its bot id (`B...`)? A
   wrong value means the mention is never recognised.

**`slack: an app token is required for Socket Mode`.**
`SLACK_APP_TOKEN` is not exported in the environment of the `hive serve` process.
Exporting it in another shell is not enough.

**`slack: chat.postMessage failed: not_in_channel`.**
The bot is not in the channel. `/invite @your-bot`.

**`slack: apps.connections.open failed: invalid_auth`.**
The app token is wrong, or Socket Mode is not enabled for the app.

**`/new_chat` does nothing at all.**
Slack intercepted it. A message that starts with `/` is Slack's, not Hive's.
Address the bot: `@your-bot /new_chat`.

**A command is reported as unknown.**
Compare against the table above. The transport recognises a command form and
reports an unknown operation as an error, rather than forwarding the text to the
agent — that is deliberate.

**A reply never appears, and the log says `chat.postMessage failed: invalid_auth`.**
Slack occasionally answers a valid token this way. It is transient: the same call
succeeds moments later. Hive does not retry a failed post, because
`chat.postMessage` has no idempotency key and a retry could duplicate a message.
Re-send your message if a reply is lost.

**A turn never finishes.**
The agent is probably waiting for permission. See *Permission buttons*.

---

## What the transport does not do

- It does not decide Hive semantics. `/new_chat` is a Slack name; the operation is
  `session.create`.
- It does not reach session internals. It normalizes Slack input into an envelope
  and hands it to Hive.
- It does not hold agent credentials. Those belong to the agent, not the
  transport.
- It is not a second business-logic system. Buttons carry the authenticated
  principal and a stable action identity; Hive performs the transition.
