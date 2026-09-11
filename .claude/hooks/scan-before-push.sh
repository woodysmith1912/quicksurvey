#!/bin/sh
# PreToolUse hook (matcher: Bash). Before any command containing "git push",
# scan everything that push could publish -- every commit reachable from a
# local branch or tag but not from any remote-tracking ref: trees, commit
# messages and author/committer identities -- for non-public data, and deny
# the push if anything is found. The index is scanned too, because the hook
# runs before the command does: a "git commit && git push" in one command
# would otherwise publish staged content no commit had yet been scanned for.
#
# "Non-public" here means:
#   * secrets: private keys, cloud and VCS tokens;
#   * infrastructure that cannot be learned by examining the public
#     deployment: public IP addresses, home directories, cluster names,
#     plus whatever deploy/private/nonpublic-patterns.txt lists;
#   * any person other than the project's owner: commit identities, e-mail
#     addresses, and names listed in the private pattern file.
#
# deploy/private/nonpublic-patterns.txt holds one extended regex per line and
# is gitignored: a list of what must not be published is itself something
# that must not be published. Without it the generic checks still run.
#
# Usage from the hook: JSON on stdin. Usage by hand:
#   .claude/hooks/scan-before-push.sh --unpushed   # what a push would send
#   .claude/hooks/scan-before-push.sh --history    # every commit, for audits
set -u

OWNER='Woody Smith <woodysmith1912@proton.me>'
# E-mail addresses that may appear in trees and messages.
ALLOWED_EMAIL='^(woodysmith1912@proton\.me|noreply@anthropic\.com)$|@([a-z0-9.-]+\.)?(example\.(com|org|net)|[a-z0-9.-]+\.invalid|[a-z0-9.-]+\.test)$'

mode=hook
case "${1:-}" in
  --unpushed) mode=unpushed ;;
  --history)  mode=history ;;
esac

if [ "$mode" = hook ]; then
  cmd=$(jq -r '.tool_input.command // ""' 2>/dev/null) || exit 0
  case "$cmd" in *"git push"*) ;; *) exit 0 ;; esac
fi

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
/home/[a-z][a-z0-9_-]*
/Users/[A-Za-z][A-Za-z0-9_-]*
\bdo-[a-z]+[0-9]-[a-z0-9-]+
\.ts\.net\b
GENERIC

private=deploy/private/nonpublic-patterns.txt
note=""
if [ -f "$private" ]; then
  grep -v -E '^[[:space:]]*(#|$)' "$private" >>"$pat" || true
else
  note=" (no $private found; only generic patterns were checked)"
fi

IPV4='\b([0-9]{1,3}\.){3}[0-9]{1,3}\b'
EMAIL='[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}'

# stdin: lines ending in ":<ip>". Keep only globally routable unicast.
public_ipv4() {
  awk -F: '{
    n = split($NF, o, ".")
    if (n != 4) next
    for (i = 1; i <= 4; i++) if (o[i] !~ /^[0-9]+$/ || o[i] > 255) next
    a = o[1] + 0; b = o[2] + 0; c = o[3] + 0
    if (a == 0 || a == 10 || a == 127 || a >= 224) next
    if (a == 169 && b == 254) next
    if (a == 172 && b >= 16 && b <= 31) next
    if (a == 192 && b == 168) next
    if (a == 192 && b == 0 && c == 2) next
    if (a == 198 && (b == 18 || b == 19)) next
    if (a == 198 && b == 51 && c == 100) next
    if (a == 203 && b == 0 && c == 113) next
    if (a == 100 && b >= 64 && b <= 127) next
    print
  }'
}

# stdin: lines ending in ":<email>". Drop the allowed ones.
foreign_email() {
  # ENVIRON rather than -v: awk processes escape sequences in -v values.
  ALLOWED_EMAIL="$ALLOWED_EMAIL" awk -F: '{ e = tolower($NF); if (e !~ ENVIRON["ALLOWED_EMAIL"]) print }'
}

case "$mode" in
  history) commits=$(git rev-list --all 2>/dev/null) ;;
  *)       commits=$(git rev-list --branches --tags --not --remotes 2>/dev/null) ;;
esac

hits=""
add() { [ -n "$1" ] && hits="$hits$1
"; }

# Staged content: what the next commit in this same command would contain.
add "$(git grep --cached -I -n -E -f "$pat" -- 2>/dev/null | sed "s/^/(index): /")"
add "$(git grep --cached -I -n -o -E "$IPV4" -- 2>/dev/null | public_ipv4 | sed "s/^/(index): /;s/\$/ (public IP)/")"
add "$(git grep --cached -I -n -o -E "$EMAIL" -- 2>/dev/null | foreign_email | sed "s/^/(index): /;s/\$/ (e-mail)/")"

for c in $commits; do
  [ -n "$c" ] || continue
  short=$(git rev-parse --short "$c")
  add "$(git grep -I -n -E -f "$pat" "$c" -- 2>/dev/null | sed "s/^$c:/$short: /")"
  add "$(git grep -I -n -o -E "$IPV4" "$c" -- 2>/dev/null | public_ipv4 | sed "s/^$c:/$short: /;s/\$/ (public IP)/")"
  add "$(git grep -I -n -o -E "$EMAIL" "$c" -- 2>/dev/null | foreign_email | sed "s/^$c:/$short: /;s/\$/ (e-mail)/")"

  msg=$(git log -1 --format=%B "$c")
  add "$(printf '%s\n' "$msg" | grep -n -E -f "$pat" 2>/dev/null | sed "s/^/$short: (commit message):/")"
  add "$(printf '%s\n' "$msg" | grep -n -o -E "$IPV4" | public_ipv4 | sed "s/^/$short: (commit message):/;s/\$/ (public IP)/")"
  add "$(printf '%s\n' "$msg" | grep -n -o -E "$EMAIL" | foreign_email | sed "s/^/$short: (commit message):/;s/\$/ (e-mail)/")"

  add "$(git log -1 --format='%an <%ae>%n%cn <%ce>' "$c" | grep -v -F -x "$OWNER" | sort -u | sed "s/^/$short: (identity):/;s/\$/ is not $OWNER/")"
done

n=$(printf '%s' "$commits" | grep -c . || true)
if [ -z "$hits" ]; then
  [ "$mode" = hook ] || echo "clean: $n commit(s) and the index scanned$note"
  exit 0
fi

# One line per distinct path:line:text, keeping the first commit it was seen in.
summary=$(printf '%s' "$hits" | awk -F': ' '{ k = $0; sub(/^[^:]*: /, "", k) } !seen[k]++' | head -n 60)
reason="Push blocked: non-public data found ($n commit(s) and the index scanned)$note.
Matches (commit or index: path:line: text):
$summary

Remove or redact it (amend, or rewrite the unpushed commits), then push again."

if [ "$mode" = hook ]; then
  jq -n --arg r "$reason" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
  exit 0
fi
printf '%s\n' "$reason"
exit 1
