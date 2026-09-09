#!/usr/bin/env python3
"""tail_journal.py — read the journal from another language, with no CLI at all.

    python3 examples/tail_journal.py [tag]

The file interface (INTERFACES.md §2) is the portable one: an append-only NDJSON
log plus a JSON heartbeat. Anything that can read a file can be a consumer.

This is a READER. It deliberately does not touch the read cursor, because
reading is not consuming — only `convo wait` advances delivery position.
"""
import calendar, json, os, sys, time

tag = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("CONVO_TAG", "default")
home = os.environ.get("AGENT_CONVERSATIONS_HOME",
                      os.path.expanduser("~/.config/agent-conversations"))
journal = os.path.join(home, "journal", f"{tag}.ndjson")
beat = os.path.join(home, f"heartbeat.{tag}.json")

with open(journal, "r", encoding="utf-8") as f:
    f.seek(0, os.SEEK_END)                       # skip the backlog; tail only
    while True:
        line = f.readline()
        if not line:
            # Liveness first: a stale heartbeat means silence is deafness,
            # not quiet. Never wait forever on a corpse.
            try:
                hb = json.load(open(beat, encoding="utf-8"))
                # timegm, NOT mktime: "ts" is UTC, and reading it as local time
                # makes a healthy daemon look hours stale (or hours in the future).
                age = time.time() - calendar.timegm(time.strptime(hb["ts"][:19], "%Y-%m-%dT%H:%M:%S"))
                if age > max(5, (hb.get("intervalMs", 0) / 1000) * 3):
                    sys.exit("listener is stale — the daemon is not delivering")
            except FileNotFoundError:
                sys.exit("no heartbeat — no daemon is running")
            time.sleep(0.25)
            continue
        if not line.endswith("\n"):               # partial write; wait for the rest
            f.seek(f.tell() - len(line))
            time.sleep(0.05)
            continue
        m = json.loads(line)
        print(f'{m["from"]["name"]} in {m["source"]["name"]}: {m["text"]}', flush=True)
