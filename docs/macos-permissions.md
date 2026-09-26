# macOS permissions

macOS guards some locations behind the Transparency, Consent, and Control (TCC)
system — `~/Desktop`, `~/Documents`, `~/Downloads`, iCloud Drive, and removable
and network volumes. A process that reads one must be granted access, and macOS
asks the user for it.

## Why the dialog names Hive

TCC attributes an access to the **responsible process**: the ancestor the system
holds accountable for the whole process tree. A child defers to its parent by
design — that is a security property, not a fault — so an agent that runs
`ls ~/Documents` produces a dialog naming the daemon that launched it, not
`hermes-acp`.

Which process that is depends on how Hive was started:

| Started by | Responsible process |
|---|---|
| `hive service install` (a launchd job) | `hive` |
| `hive serve` in a terminal | that terminal |

You can see it directly:

```sh
sudo launchctl procinfo $(pgrep -x hive-node) | grep responsible
```

## Grant it once

Because TCC checks the responsible process, **one grant covers every agent Hive
launches**:

1. Open **System Settings → Privacy & Security → Full Disk Access**.
2. Add `~/.hive/data/bin/hive` — the daemon binary the service runs.
3. Restart it: `hive service restart`.

No dialog appears for those folders again.

If you run `hive serve` from a terminal instead, grant the terminal — the
responsible process is what TCC asks, and it is not Hive in that case.

## Or keep the workspace out of the way

Hive's workspace is the root an agent can read and write. Somewhere outside the
guarded folders — `~/.hive/workspace` (the default), `~/Developer`, `~/src` —
needs no permission at all, because nothing protected is read.

```toml
workspace_dir = "/Users/you/Developer"
```

`hive doctor` warns when the configured workspace is inside a guarded folder, so
the dialog is expected rather than surprising.

## What Hive does not do

Hive does not launch agents with responsibility *disclaimed*, which would make
the dialog name the agent instead of Hive.

The API for that, `responsibility_spawnattrs_setdisclaim`, is undocumented (LLDB,
Chromium and Qt use it), and it would trade a dialog you can answer for access you
may not get: TCC records a grant against an *identifiable* requester, and the
agent binaries are unsigned CLI tools. An unattributable request is denied rather
than prompted, which is a worse failure than a dialog with the wrong name on it.

The same reasoning is why Hive does not hand an agent a directory the user chose
from chat: the workspace is the machine owner's decision, made once, and the
permission story above is what it costs to keep it that way.
