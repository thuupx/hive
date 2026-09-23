#!/usr/bin/env python3
"""Measure what the Hive daemon and its plugins cost while they run.

It samples the whole process tree rooted at the daemon, so the node child and the
plugin processes are counted with it. Growth across samples is what a leak looks
like: a steady-state daemon should be flat.
"""

import os
import subprocess
import sys
import time

SAMPLE_SECONDS = int(os.environ.get("SAMPLE_SECONDS", "20"))
SAMPLES = int(os.environ.get("SAMPLES", "10"))
ROOT = os.environ.get("HIVE_PID")


def children(pid):
    out = subprocess.run(["ps", "-eo", "pid=,ppid="], capture_output=True, text=True).stdout
    kids = {}
    for line in out.splitlines():
        parts = line.split()
        if len(parts) != 2:
            continue
        child, parent = parts
        kids.setdefault(parent, []).append(child)
    return kids


def tree(root, kids):
    seen, stack = [], [root]
    while stack:
        pid = stack.pop()
        seen.append(pid)
        stack.extend(kids.get(pid, []))
    return seen


# Hive's own processes. An agent runtime the user installed is their cost, not
# Hive's, and counting it would drown the number that matters.
HIVE_PREFIXES = ("hive", "hive-plugin-")


def is_hive(command):
    parts = command.split()
    if not parts:
        return False
    return os.path.basename(parts[0]).startswith(HIVE_PREFIXES)


def stats(pids):
    out = subprocess.run(
        ["ps", "-o", "pid=,rss=,%cpu=,command=", "-p", ",".join(pids)],
        capture_output=True, text=True).stdout
    rows = []
    for line in out.splitlines():
        parts = line.split(None, 3)
        if len(parts) < 4:
            continue
        if not is_hive(parts[3]):
            continue
        rows.append((parts[0], int(parts[1]), float(parts[2]), parts[3]))
    return rows


def main():
    root = ROOT
    if not root:
        print("set HIVE_PID to the daemon pid")
        return 1

    print(f"sampling every {SAMPLE_SECONDS}s, {SAMPLES} times, tree rooted at {root}")
    print("counting only hive and hive-plugin-* processes\n")
    header = f"{'elapsed':>8} {'rss MB':>8} {'cpu %':>7} {'procs':>6}  largest"
    print(header)
    print("-" * len(header))

    first = None
    samples = []
    started = time.time()

    for i in range(SAMPLES):
        rows = stats(tree(root, children(root)))
        total_rss = sum(r[1] for r in rows) / 1024
        total_cpu = sum(r[2] for r in rows)
        largest = max(rows, key=lambda r: r[1], default=("", 0, 0, ""))

        if first is None:
            first = total_rss
        samples.append(total_rss)

        name = os.path.basename(largest[3].split()[0]) if largest[3] else "?"
        print(f"{time.time() - started:7.0f}s {total_rss:8.1f} {total_cpu:7.1f} "
              f"{len(rows):6}  {name} {largest[1] / 1024:.1f} MB")

        if i < SAMPLES - 1:
            time.sleep(SAMPLE_SECONDS)

    print()
    print(f"first sample: {first:.1f} MB")
    print(f"last sample:  {samples[-1]:.1f} MB")
    print(f"growth:       {samples[-1] - first:+.1f} MB "
          f"({(samples[-1] - first) / max(first, 1) * 100:+.1f}%)")

    print("\nper process, last sample:")
    for pid, rss, cpu, command in sorted(stats(tree(root, children(root))),
                                         key=lambda r: -r[1]):
        print(f"  {rss / 1024:7.1f} MB  {cpu:5.1f}%  pid={pid:<7} {command[:70]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
