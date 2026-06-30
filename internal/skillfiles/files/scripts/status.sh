#!/usr/bin/env bash
# Print shunt status for the current repo and the recommended next command.
# Run from inside a shunt repo (or a siding). Override the binary with SHUNT_BIN.
set -euo pipefail
BIN="${SHUNT_BIN:-shunt-dev}"

json="$("$BIN" active --json 2>/dev/null || true)"
if [ -z "$json" ] || ! printf '%s' "$json" | grep -q '"active": *true'; then
  echo "Not a shunt app here. Register with: $BIN app add"
  exit 0
fi

tmp="$(mktemp)"
printf '%s' "$json" > "$tmp"
BIN="$BIN" JSON="$tmp" python3 <<'PY'
import json, os
b = os.environ["BIN"]
d = json.load(open(os.environ["JSON"]))
print(f"shunt app: {d['project']}")
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
    if not s["appRunning"]:
        print(f"      next: {b} up {s['name']}")
    elif not s["live"]:
        print(f"      next: {b} switch {s['name']}")
    else:
        print("      next: serving (nothing to do)")
PY
rm -f "$tmp"
