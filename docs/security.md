# Security model

Hive secures system boundaries, not agent tool semantics. The underlying agent
owns tool authorization; Hive relays what the agent asks for.

## Identity classes

```text
User     an external identity mapped to a Hive principal
Node     a machine that runs agents
Plugin   a local process that integrates something
```

## Users

External identities are mapped to principals, and a principal must be named
before it can act:

```text
slack:U123
telegram:123
web:<client-id>
```

```toml
[security]
allowed_users = ["slack:U123"]
```

**Unknown access is denied by default.** For a personal installation the default
model is the owner principal, which is the local caller on the owner-only unix
socket.

Authentication establishes the principal; authorization decides what it may do.
The protocol never treats an unverified, client-supplied principal string as
proof of identity:

- A plain client acts as the identity its connection established, whatever it
  puts in a message.
- A trusted transport may assert a principal per message, but only one prefixed
  with its own transport name. `slack` may assert `slack:U123`; it may not assert
  `telegram:1`.
- A plugin acting as itself is an authenticated local process whose capabilities
  the core already granted. That is a different thing from being an allowed user,
  and it does not let the plugin act as one.

A session a principal cannot reach is reported as **not found**, not as
unauthorized, so a principal cannot learn that a session it cannot reach exists.

## Nodes

Nodes use persistent public-key identity. The coordinator records the node
identity and establishes an authenticated connection before accepting node
operations.

Node authentication and node authorization are separate: an authenticated node is
not automatically an authorized one. **Enrollment, revocation, and key rotation
are not implemented in v1.** v1 runs the node on the same machine as the
coordinator and shares the locally generated certificate, so the node link is
encrypted and authenticated against that certificate rather than against a
public CA.

Every authenticated node connection has a connection generation. Reconnecting
creates a new generation, and the previous connection stops being accepted for
commands.

## Plugins

V1 plugins are trusted local processes. **Process isolation provides fault
containment, not a security sandbox.** A trusted plugin has powerful access to
Hive through its plugin protocol.

A plugin declares the capabilities it wants. The core intersects that with what
its plugin type may ever hold, and checks every inbound call against the granted
set. Registration is descriptive and never a grant.

A trusted plugin identity and a plugin process instance are distinct: the identity
keeps its trust, while each process instance gets a fresh connection generation,
and a superseded instance cannot keep invoking capabilities.

**Enrollment, revocation, and key rotation for plugins are not implemented in
v1.** Trust is a configuration decision.

## Transport bindings

A transport binding records which principal a conversation belongs to. Session
isolation is built on that: a non-owner reaches a session only through a binding
it holds.

## Permissions

Hive does not implement a tool-permission engine. A permission request is opaque
protocol data except for the metadata required to route, expire, authorize, and
correlate the response.

A permission request is a one-time state transition. Once resolved it cannot
transition again, concurrent responses are resolved by a single atomic
compare-and-set, and a timeout is a safe terminal state.

**A pending request never becomes an implicit approval**, including when the
coordinator connection is lost. If a request cannot be safely recovered, it fails
closed rather than authorizing an unknown action.
