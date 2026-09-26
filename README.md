# Hive

**A self-hosted gateway between where you talk and the agents that do the work.**

Slack on one side, anything that speaks [ACP](https://agentclientprotocol.com)
on the other — Claude Code, Codex, Devin, Gemini. You bring the agents. Hive
keeps the conversation.

---

## The problem

- **The agent is not where you are.** It runs where the code is, so you SSH in
  or give up.
- **The conversation is trapped in the tool.** Want a second agent mid-thread?
  Start over. The context dies with the session.
- **It is not really yours.** State lives in the agent or in Slack. You cannot
  query it, and you cannot audit it.

## Why today's tools stop short

**Slack-as-ACP bridges** (`slack-acp-bridge`, `hydra-acp/slack`, ...) — a Slack
thread *is* the agent session. Shortest path to value, but swapping the agent is
a rewrite, and the state is visible only through Slack.

**OpenACP** — broader (Telegram/Discord/Slack, 28+ agents, npm channel plugins),
but still a bridge: one conversation maps to one agent session, on one host.

**Vendor clients** — great with one agent, silent about the rest.

All of them weld **transport, conversation, and execution** into one object.
That is what makes them quick to build and hard to grow.

## The idea

Hive separates the three.

```text
Transport   presentation      Slack, a terminal, later: anything
Session     the conversation  Hive: durable, yours, transport-agnostic
AgentRun    execution         one agent process, on one node
```

> Hive owns orchestration and context. Agents own execution and tool semantics.
> Transports own presentation.

What that unlocks:

- **Handoff** — give a session to another agent mid-conversation. The context
  comes with it; the Slack thread does not change.
- **Durable sessions** — an event log on disk, not a variable in a bot. Restart
  the daemon and the history is still there, readable from any client.
- **Execution where the code lives** — coordinator and node are separate roles
  speaking a real protocol, even on one machine. Multi-machine is a deployment
  choice, not a rewrite.
- **Agents as config, not code** — no per-vendor plugin. One `hive-plugin-acp`,
  and an agent is a `[agents.<name>]` entry with a command.
- **Permissions fail closed** — an agent's request is relayed and waits. Nothing
  is approved on your behalf.

## How Hive compares

| | Slack-as-ACP bridge | OpenACP | Vendor client | **Hive** |
|---|---|---|---|---|
| Talks to | one agent | many agents | one agent | many agents, one session |
| Conversation lives in | the thread | the thread | the vendor | Hive, durably |
| Switch agent mid-conversation | start over | start over | n/a | **handoff** |
| Execution | the one host | the one host | the vendor | coordinator + node |
| Extend by | forking the bot | npm channel adapter | — | process-isolated plugins |
| Your data | your machine | your machine | theirs | your machine |

Hive is more machinery than a bridge — the honest trade. If you want one agent
in Slack by lunchtime, use a bridge. If you want a conversation you can hand
between agents and still read in a year, the structure is the point.

## Quick start

Needs Go 1.27+ with CGO, a C toolchain, and an ACP agent on your `PATH`.

```sh
make build                 # bin/hive, hive-plugin-acp, hive-plugin-slack
./bin/hive init            # finds your agents, writes ~/.hive/config.toml
./bin/hive serve           # runs the coordinator, a node, and your agents
```

Then, from another terminal:

```sh
./bin/hive agent list
./bin/hive session create
./bin/hive session prompt <session-id> "explain this repository"
./bin/hive session handoff <session-id> <agent-id>
```

Connect Slack (Socket Mode — no public URL) per [docs/slack.md](docs/slack.md):

```text
@your-bot /new_chat            create a session
@your-bot fix the login bug    text becomes a prompt
@your-bot /handoff <agent>     hand it to another agent
```

Full setup, every command, and troubleshooting: **[docs/reference.md](docs/reference.md)**.

## Status

v1 is deliberately narrow: **single machine, Slack, ACP agents, CLI/TUI as a
client.** Handoff and durable sessions are in; failover, multi-machine nodes,
more transports, and a web UI are designed but not shipped.

## Documentation

- [docs/reference.md](docs/reference.md) — install, run, commands, Slack, troubleshooting
- [docs/protocol.md](docs/protocol.md) — protocol, versioning, event types
- [docs/security.md](docs/security.md) — identity, authorization, permissions
- [docs/configuration.md](docs/configuration.md) — every config key
- [docs/failure-semantics.md](docs/failure-semantics.md) — what is and is not claimed
- [hive-solution-design-v6.md](hive-solution-design-v6.md) — the architecture
- [hive-v1-implementation-plan.md](hive-v1-implementation-plan.md) — v1 cut line
- [AGENTS.md](AGENTS.md) — conventions for changing the code
