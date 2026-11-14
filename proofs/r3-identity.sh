#!/bin/sh
# R3: where the signed-in identity can be read. Prints key NAMES only.
# Run once per account: CLAUDE_CONFIG_DIR=<dir> sh proofs/r3-identity.sh
set -eu
if [ -n "${CLAUDE_CONFIG_DIR:-}" ]; then f="$CLAUDE_CONFIG_DIR/.claude.json"; else f="$HOME/.claude.json"; fi
echo "file: $f"
python3 - "$f" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception as e:
    print("cannot read:", type(e).__name__); sys.exit(0)
acct = d.get("oauthAccount")
print("oauthAccount keys:", sorted(acct.keys()) if isinstance(acct, dict) else None)
PY
echo "claude auth status keys:"
claude auth status 2>/dev/null | python3 -c 'import json,sys; print(sorted(json.load(sys.stdin).keys()))' || echo "  (not JSON)"
echo "Expected in oauthAccount: emailAddress, accountUuid, organizationUuid."
echo "Record whether organizationName and any plan/subscription key are present."
