#!/usr/bin/env bash
# Print shunt status for the current repo or siding and the recommended next command.
# Run from inside a shunt repo (or a siding). Override the binary with SHUNT_BIN.
set -euo pipefail
BIN="${SHUNT_BIN:-{{shunt-command}}}"

json="$("$BIN" active --json)"
if [ -z "$json" ]; then
  echo "No Shunt state here. Create a siding with: $BIN new <name>"
  exit 0
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
printf '%s' "$json" > "$tmp"
BIN="$BIN" JSON="$tmp" python3 <<'PY'
import json, os
b = os.environ["BIN"]
d = json.load(open(os.environ["JSON"]))
managed = d.get("managed", d.get("active", False))
if not managed:
    print(f"No Shunt state here. Create a siding with: {b} new <name>")
    raise SystemExit(0)
registered = d.get("registered", d.get("active", False))
kind = "app" if registered else "worktree-only project"
print(f"shunt {kind}: {d['project']}")
if not registered:
    print(f"  guest runtime is not registered  ->  add .shunt.app.json, then {b} app add before up")
sids = d.get("sidings", [])
if not sids:
    print(f"  no sidings yet  ->  {b} new <name>")
for s in sids:
    flags = []
    if s["live"]: flags.append("live")
    if s["appRunning"]: flags.append("running")
    elif s["guestRunning"]: flags.append("guest-up")
    label = ",".join(flags) or "idle"
    print(f"  {s['name']:<16} [{label}]")
    print(f"      edit: {s['src']}")
    probe_error = s.get("probeError", "")
    if probe_error:
        print(f"      probe error: {probe_error}")
    elif s["guestRunning"] and not s["appRunning"]:
        def route_label(r):
            port = r.get("guestPort", 0)
            return f"{r['key']}({port if port > 0 else 'no guestPort'})"
        waiting = [route_label(r) for r in s.get("routes") or []
                   if not r.get("optional") and not r.get("listening")]
        if waiting:
            print(f"      waiting on: {', '.join(waiting)}")
    if not registered:
        print("      next: edit and test in this worktree")
    elif not s["appRunning"]:
        print(f"      next: {b} up {s['name']}")
    elif not s["live"]:
        print(f"      next: {b} switch {s['name']}")
    else:
        print("      next: serving (nothing to do)")
PY
