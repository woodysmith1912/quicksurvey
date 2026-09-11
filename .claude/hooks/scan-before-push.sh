#!/bin/sh
# PreToolUse hook (matcher: Bash). Before any command containing "git push",
# scan everything that push could publish -- every commit reachable from a
# local branch or tag but not from any remote-tracking ref, trees and commit
# messages -- for non-public data, and deny the push if anything matches.
#
# Patterns come from two places:
#   * a short generic list below (private keys, cloud and VCS tokens);
#   * deploy/private/nonpublic-patterns.txt, one extended regex per line,
#     which is gitignored because the list of what must not be published is
#     itself something that must not be published.
set -u

cmd=$(jq -r '.tool_input.command // ""' 2>/dev/null) || exit 0
case "$cmd" in *"git push"*) ;; *) exit 0 ;; esac

top=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
cd "$top" || exit 0

pat=$(mktemp) || exit 0
trap 'rm -f "$pat"' EXIT
cat >"$pat" <<'GENERIC'
-----BEGIN [A-Z ]*PRIVATE KEY-----
AKIA[0-9A-Z]{16}
gh[pousr]_[A-Za-z0-9]{30,}
github_pat_[A-Za-z0-9_]{40,}
dop_v1_[0-9a-f]{40,}
xox[baprs]-[A-Za-z0-9-]{20,}
GENERIC

private=deploy/private/nonpublic-patterns.txt
note=""
if [ -f "$private" ]; then
  grep -v -E '^[[:space:]]*(#|$)' "$private" >>"$pat" || true
else
  note=" (no $private found; only generic patterns were checked)"
fi

commits=$(git rev-list --branches --tags --not --remotes 2>/dev/null) || exit 0
[ -n "$commits" ] || exit 0

hits=""
for c in $commits; do
  h=$(git grep -I -n -E -f "$pat" "$c" -- 2>/dev/null | sed "s/^$c:/${c%${c#???????}}: /")
  [ -n "$h" ] && hits="$hits$h
"
  m=$(git log -1 --format=%B "$c" | grep -n -E -f "$pat" 2>/dev/null | sed "s/^/${c%${c#???????}}: (commit message):/")
  [ -n "$m" ] && hits="$hits$m
"
done

[ -n "$hits" ] || exit 0

# One line per distinct path:line:content, keeping the first commit it was seen in.
summary=$(printf '%s' "$hits" | awk -F': ' '!seen[$2]++' | head -n 40)
n=$(printf '%s\n' "$commits" | wc -l | tr -d ' ')
reason="Push blocked: non-public data found in $n unpushed commit(s)$note.
Matches (commit: path:line: text):
$summary

Remove or redact it (amend, or rewrite the unpushed commits), then push again."
jq -n --arg r "$reason" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
exit 0
