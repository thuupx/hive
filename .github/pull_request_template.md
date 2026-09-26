<!--
Title: a conventional prefix (feat:, fix:, deps:, docs:, refactor:), lowercase,
imperative, and about the why rather than the what.

Delete any section that does not apply. Three honest lines beat thirty empty ones.
-->

## What and why

<!-- What changes, and what problem it solves. The "why" is the part a reader
     cannot recover from the diff. -->

## Verification

<!-- How you know it works. `make ci` is the floor, not the answer.

     Anything that crosses a wire — the protocol, a transport, a plugin — needs to
     have been run against something real, and what you saw needs to be in this
     box. Unit tests have never once caught a malformed permission response, a
     replayed conversation, or a message Slack refuses to accept. -->

- [ ] `make ci` passes
- [ ] Run against a real agent or platform, and here is what I saw:

## Tests

<!-- A bug fix carries a test that fails without the fix, and saying you checked
     is what makes it a regression test rather than a description of one. -->

- [ ] Added or updated tests
- [ ] Confirmed the new test fails against the old behaviour

## Semantics

<!-- Hive owns orchestration and context; agents own execution; transports own
     presentation. A change to a semantic, to `protocol/hive/v1`, or to the
     configuration format is a compatibility decision — say which, and whether the
     design document (`docs/hive-solution-design.md`) agrees or is silent. -->

- [ ] No semantic, protocol, or configuration change
- [ ] Described above, and consistent with the design document

## Layering

- [ ] `protocol/hive/v1` still imports only the standard library
- [ ] Domain packages (`internal/agent`, `internal/command`, `internal/event`,
      `internal/session`) still do not import `internal/storage`
- [ ] No platform-specific command registry or vendor semantics reached the core
- [ ] Read-modify-write goes through `UpdateAgentRunWith`, `UpdateSessionWith`, or
      `UpdateHandoffWith`

## Reaching the machine

<!-- Hive runs agents that read and write files, and asks users to approve what
     they do. If this changes either, a reviewer needs to know. -->

- [ ] Does not change what an agent can reach, or what a user can approve
- [ ] Does change it, deliberately, and the bound is described above

## Dependencies

<!-- A new dependency is a decision, not an implementation detail. -->

- [ ] No new dependencies
- [ ] New dependency, and why it beats what is already here:

## Follow-ups

<!-- What this deliberately leaves out, so a reviewer does not have to guess
     whether it was forgotten. -->
