# Hive

**A self-hosted gateway that puts your coding agents where you already work —
and keeps the conversation, not the tool, at the center.**

Hive sits between the places you talk (Slack, your terminal) and the agents that
do the work (anything that speaks [ACP](https://agentclientprotocol.com) —
Claude Code, Codex, Devin, Gemini, and friends). You bring the agents. Hive
brings the conversation that survives them.

---

## The problem

Coding agents got good. Talking to them did not.

Every agent ships its own CLI, its own session, its own idea of what "the
conversation" is. You end up with three separate problems that look like one:

- **You have to be at the machine.** The agent runs where the code is, which is
  rarely where you are. So you SSH in, or open a tunnel, or just give up and
  wait until you are back at your desk.
- **The conversation is trapped in the tool.** You had a great thread with one
  agent. Now you want a second opinion, or a cheaper model for the cleanup
  pass. You start over. The context dies with the session.
- **It is not really yours.** Session state lives inside the agent, or inside
  Slack, or inside whatever glue script someone wrote. When it breaks you cannot
  see why, and when you want to audit it you cannot.

If you have ever asked an agent to do something from your phone, watched it
work, and then realized you have no way to ask a *different* agent to take over
— that is the problem Hive is built for.

## What people build today, and where it stops

There is a healthy ecosystem of tools that connect chat to agents. They are
good, and Hive learned from them. They also share a ceiling.

**Slack-as-an-ACP-client bridges** — things like `slack-acp-bridge`,
`hydra-acp/slack`, and the many others in the ACP client list. A Slack bot
speaks ACP straight to one agent. An `@mention` opens a thread; the thread *is*
the agent session.

This is a lovely design, and it is the shortest path from zero to "my agent is
in Slack." But the identity is fused: one Slack thread equals one agent session.
Swapping the agent mid-thread is not a feature, it is a rewrite. The state lives
in Slack and a local SQLite file, so it is visible only through the transport
that created it. And it runs on the one machine that can reach the agent.

**OpenACP** — a self-hosted bridge with a much broader reach: Telegram, Discord,
and Slack, plus 28+ agents discovered from the ACP registry, with an npm plugin
system for channels. It is the most complete "bridge" out there today, and its
setup experience is genuinely pleasant.

It is still a bridge. A platform conversation maps to a single agent session.
Everything runs on one host, and the plugin surface is a channel adapter. There
is no separate notion of a session that outlives an agent, and no execution
plane that is distinct from the control plane.

**Vendor clients** — each agent's own app. Great with that agent. Silent about
every other one.

The pattern across all of them: **transport, conversation, and execution are the
same object.** That is what makes them quick to build and hard to grow.

## The idea

Hive separates three things the others weld together.

```text
Transport   owns presentation        (Slack, a terminal, later: anything)
Session     owns the conversation    (Hive: durable, yours, transport-agnostic)
AgentRun    owns execution           (one agent process, on one node)
```

Or, as the design document puts it:

> Hive owns orchestration and context. Agents own execution and tool semantics.
> Transports own presentation and delivery. Plugins own integration boundaries.

Once a conversation is not an agent session, a few things that were hard become
ordinary:

- **Handoff.** Mid-conversation, give the session to a different agent. The
  context comes with it. The Slack thread does not change. The old agent's
  session is untouched — Hive keeps the runtime sessions separate and never
  pretends one vendor's session is another's.
- **Durability by default.** A session is an event-sourced record on disk, not a
  variable in a bot process. Restart the daemon, reconnect, and the history is
  still there — inspectable with `hive session events`, from any client, not
  just the one that started it.
- **Execution where the code lives.** The coordinator (the brain) and the node
  (the hands) are separate roles speaking a real protocol, even on one machine.
  That is what makes "run the agent on the beefy box" a deployment choice rather
  than a rewrite. Today v1 runs them together; the seam is already load-bearing.
- **Agents as configuration, not as code.** Hive ships no agent and no
  vendor-specific plugin. There is one `hive-plugin-acp`, and an agent is a
  `[agents.<name>]` entry with a command. A new ACP agent works because it
  speaks ACP, not because someone wrote a Hive plugin for it.
- **Permissions that fail closed.** When an agent asks to run a tool, Hive
  relays the request and waits. Nothing is silently approved on your behalf.
  With Slack connected the request shows up as buttons; the CLI is the fallback.

The result is less "a bot for one agent" and more "a switchboard for all of
them."

## How Hive compares

| | Slack-as-ACP bridge | OpenACP | Vendor client | **Hive** |
|---|---|---|---|---|
| Talks to | one agent per install | many agents | one agent | many agents, one session |
| The conversation lives in | the chat thread | the chat thread | the vendor | Hive, durably |
| Switch agent mid-conversation | start over | start over | n/a | **handoff** |
| Where execution runs | the one host | the one host | the vendor | coordinator + node, designed for more |
| Extend by | forking the bot | npm channel adapter | — | transport or agent plugin, process-isolated |
| Your data | your machine | your machine | theirs | your machine |
| Readable history | the thread | the thread | the app | event log, queryable from any client |

Hive is more machinery than a bridge, and that is the honest trade. If you want
"my one agent in Slack by lunchtime," a bridge is the right tool. If you want a
conversation you can hand between agents, run across machines, and still read in
a year, the extra structure is the point.

## Quick start

You need Go 1.27+ with CGO enabled, a C toolchain, and at least one ACP agent on
your `PATH`.

```sh
make build                 # bin/hive, bin/hive-plugin-acp, bin/hive-plugin-slack
./bin/hive init            # finds your ACP agents, writes ~/.hive/config.toml
./bin/hive serve           # runs the coordinator, a node, and your agents
```

In another terminal, the CLI talks to the running daemon:

```sh
./bin/hive agent list      # what this installation can run
./bin/hive session create  # start a conversation
./bin/hive session prompt <session-id> "explain this repository"
./bin/hive session events <session-id>
```

Hand a conversation to a different agent, mid-thread:

```sh
./bin/hive session handoff <session-id> <agent-id>
```

Connect Slack (Socket Mode — no public URL, no tunnel) by following
[docs/slack.md](docs/slack.md), then talk to it where you already are:

```text
@your-bot /new_chat            create a session
@your-bot fix the login bug    ordinary text becomes a prompt
@your-bot /handoff <agent>     hand the session to another agent
```

Full setup, every command, configuration keys, and troubleshooting live in
**[docs/reference.md](docs/reference.md)**.

## Status

Hive is at v1, which is deliberately narrow: **single machine, Slack as the one
production transport, ACP agents, CLI/TUI as a protocol client.** Handoff and
durable sessions are in. Coordinator failover, multi-machine nodes, more
transports, and a web UI are designed but not shipped — the boundaries exist so
they can arrive without a rewrite.

This is an early, personal-scale system. The design document and the
implementation plan are the source of truth for what is promised and what is
explicitly not.

## Documentation

- **[docs/reference.md](docs/reference.md)** — install, run, commands, Slack,
  troubleshooting
- [docs/protocol.md](docs/protocol.md) — the Hive protocol: domains, versioning,
  event types
- [docs/security.md](docs/security.md) — identity, authorization, permissions
- [docs/configuration.md](docs/configuration.md) — every configuration key
- [docs/failure-semantics.md](docs/failure-semantics.md) — what is guaranteed,
  what is best-effort, what is not claimed
- [docs/slack.md](docs/slack.md) — the Slack transport in full
- [hive-solution-design-v6.md](hive-solution-design-v6.md) — the architecture
- [hive-v1-implementation-plan.md](hive-v1-implementation-plan.md) — the v1 cut
  line and milestones
- [AGENTS.md](AGENTS.md) — conventions for changing this codebase
