#!/bin/sh
# R2: hooks and a status line passed with --settings for one launch fire.
set -eu
T=$(mktemp -d)
cat > "$T/settings.json" <<JSON
{
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "echo stop >> '$T/fired'"}]}]
  },
  "statusLine": {"type": "command", "command": "echo statusline >> '$T/fired'; echo CS-R2-MARKER"}
}
JSON
: > "$T/fired"
claude -p --settings "$T/settings.json" "Reply OK." >/dev/null
if grep -q '^stop$' "$T/fired"; then echo "R2 hooks: PASS"; else echo "R2 hooks: FAIL"; fi
echo
echo "The status line only runs interactively. Run this, send one message, and look"
echo "for CS-R2-MARKER at the bottom of the screen, then quit:"
echo "  claude --settings '$T/settings.json'"
printf "Did CS-R2-MARKER appear? [y/n] "
read -r a
case "$a" in y*|Y*) echo "R2 status line: PASS" ;; *) echo "R2 status line: FAIL" ;; esac
