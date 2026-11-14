#!/bin/sh
# R1: a session started under one config directory resumes under another
# that shares the same projects/ directory, keeping its session id.
# Needs two signed-in accounts. Spends a few tokens on each.
set -eu
T=$(mktemp -d)
STORE="${CLAUDE_STORE:-$HOME/.claude}"
mkdir -p "$STORE/projects" "$T/a" "$T/b" "$T/proj dir"
ln -s "$STORE/projects" "$T/a/projects"
ln -s "$STORE/projects" "$T/b/projects"

echo "Sign in to two DIFFERENT accounts, one per command, in another terminal:"
echo "  CLAUDE_CONFIG_DIR='$T/a' claude auth login"
echo "  CLAUDE_CONFIG_DIR='$T/b' claude auth login"
printf "Press Enter when both are signed in. "
read -r _

ID=$(uuidgen | tr 'A-Z' 'a-z')
cd "$T/proj dir"
echo "--- start under A (session $ID)"
CLAUDE_CONFIG_DIR="$T/a" claude -p --session-id "$ID" "Remember the word heron. Reply OK."

count_files() { find "$STORE/projects" -name '*.jsonl' -newer "$T/a/projects" 2>/dev/null | wc -l | tr -d ' '; }
before=$(count_files)

echo "--- resume under B by id"
if out=$(CLAUDE_CONFIG_DIR="$T/b" claude -p --resume "$ID" "What word did I ask you to remember? One word." 2>&1); then :; fi
echo "$out"
after=$(count_files)
same=$(find "$STORE/projects" -name "$ID.jsonl" | wc -l | tr -d ' ')

if echo "$out" | grep -qi heron && [ "$same" = 1 ] && [ "$before" = "$after" ]; then
  echo "R1 PASS: resumed under B, same session id, one transcript"
else
  echo "R1 FAIL by id (heron in answer: $(echo "$out" | grep -qi heron && echo yes || echo no); transcripts named $ID: $same; new transcripts: $((after - before)))"
  path=$(find "$STORE/projects" -name "$ID.jsonl" | head -1)
  echo "--- retry under B by transcript path: $path"
  if out2=$(CLAUDE_CONFIG_DIR="$T/b" claude -p --resume "$path" "What word did I ask you to remember? One word." 2>&1); then :; fi
  echo "$out2"
  if echo "$out2" | grep -qi heron; then echo "R1 by path: PASS"; else echo "R1 by path: FAIL"; fi
fi
echo "Sign out when done:"
echo "  CLAUDE_CONFIG_DIR='$T/a' claude auth logout; CLAUDE_CONFIG_DIR='$T/b' claude auth logout; rm -rf '$T'"
