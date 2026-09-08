# QuickSurvey — design

## What it is

A small web application for running "which of these are you interested in?"
surveys. A survey is a list of options; a respondent gives a thumbs-up to as
many as they like. You hand out one URL. Results go out as TSV for Google
Sheets.

Two audiences, one binary:

- **Respondents** open `/s/<id>`. No account, no name, no email.
- **Editors and viewers** sign in at `/login` and work under `/admin/`.

## Constraints that shaped it

| Constraint | Consequence |
|---|---|
| Go, single container | One static binary; templates and CSS embedded with `embed.FS`, zone database compiled in with `time/tzdata` |
| No database | State is files under one data directory, mirrored in memory |
| Responses anonymous | Nothing identifying is stored — no account link, no IP, no user agent |
| Cookies deter repeat voting | A random browser token, keyed and hashed per survey before storage |
| Export to Google Sheets | TSV, because Sheets imports it without a dialect prompt |
| Behind a reverse proxy | Plain HTTP inside the container; `Secure` cookies; honours `X-Forwarded-*` |
| Distroless, rootless, read-only | No shell, no libc, no package manager; uid 65532; `/data` is the only writable path, and temp files are created inside it rather than `/tmp` |

## The image

`gcr.io/distroless/static-debian12:nonroot`, about 11 MB. The only executable
in it is `quicksurvey`, which forces two design choices:

- **Health check.** There is no `wget` or `curl` to probe with, so the binary
  answers its own: `quicksurvey healthcheck` is the `HEALTHCHECK` command, and
  it is the only place in the program that makes an outbound request.
- **Seeding a test instance.** There is no shell to chain `user add && serve`,
  so the compose test profile uses a separate init container that must exit
  successfully before the server starts.

`/data` is created in the image owned by 65532, so a fresh named volume
inherits that ownership rather than being created root-owned and unwritable.

`no-new-privileges` is deliberately off. Under snap-packaged Docker the daemon
runs confined by AppArmor, so every container start needs a profile transition
at `execve`, which `no_new_privs` forbids — containers die immediately, for any
image. The flag is available as an opt-in overlay for hosts where it works, and
costs little when absent: there is no setuid binary in the image to escalate
through.

## Storage

```
$QS_DATA_DIR/
  secret.key                     32 random bytes, generated on first start
  users.json                     accounts: name, role, PBKDF2 verifier
  surveys/<id>/survey.json       definition — rewritten atomically
  surveys/<id>/responses.jsonl   append-only, one JSON object per line
```

Definitions are small and change rarely, so they are rewritten whole via
temp-file-plus-rename: a reader or a crash never sees half a file. Responses are
appended and never rewritten, so a hard kill can lose at most the last line;
`loadResponses` skips a torn line rather than refusing to start.

Everything is also held in memory and reads are served from there. The files are
the durable copy, replayed at startup. This is the trade the "no database"
constraint buys: no query language, no concurrent writers, and a working set
that must fit in RAM. For the scale this is built for — surveys with hundreds to
low thousands of respondents — that is not a limit anyone will meet.

**A single process must own the data directory.** There is no file locking. Do
not run two replicas against one volume.

## Anonymity, and what "prevent casual multiple voting" actually means

On first visit a respondent's browser is given `qs_voter`, a random 128-bit
token. It is meaningless on its own and is never stored server-side. What gets
stored is

```
voter_id = HMAC(secret.key, "voter" ‖ survey_id ‖ token)[:22]
```

Two properties follow. The same browser produces a *different* voter id on every
survey, so responses cannot be correlated across surveys even by someone holding
the data directory. And the token cannot be recovered from a stored response, so
the data directory cannot be turned back into a list of browsers.

This stops someone voting twice by reloading the page. It does not stop a
private window, a second browser, or a cleared cookie jar — and it is not meant
to. Anything stronger needs identity, which the requirement rules out.

Repeat submissions **replace** the previous answer rather than being refused, so
a misclick is fixable and the count still cannot be inflated.

## Options, write-ins and merges

Responses reference options by ID and the log is append-only, so an option is
never deleted from a survey. Its `status` changes instead:

- `approved` — on the ballot, counts in the tally
- `pending` — a write-in awaiting moderation: visible only to the person who
  proposed it, counts for nobody yet
- `rejected` / `removed` — off the ballot, still in the export
- `merged` — folded into another option; `Resolve` follows the pointer so the
  vote lands on the target

That last one is why merging is cheap: no stored response is ever rewritten.
`Resolve` is bounded by the option count, so a cyclic merge chain (which the
moderation handler already refuses to create) resolves to "not counted" rather
than looping.

A respondent's vote for their own pending write-in is recorded immediately and
starts counting the moment a moderator approves it — they do not have to come
back.

## Authentication

Passwords are PBKDF2-HMAC-SHA256, 600,000 iterations, per-user salt, stored in a
self-describing string so the cost can be raised later without invalidating
accounts. Unknown usernames still pay for one verification, so response time
does not enumerate accounts.

Sessions are stateless signed cookies: `name|expiry|HMAC(secret, name|expiry,
password_hash)`. Including the password hash in the MAC means changing or
resetting a password signs that user out everywhere, with no session table to
keep. A restart does not sign anyone out, because the key is on disk.

CSRF tokens are derived from whichever cookie identifies the caller — the
session for accounts, the voter token for respondents. Both cookies are
`HttpOnly`, so a cross-site page cannot read the token it would need to forge a
request.

## Invitations and approval

Adding an administrator has two halves, deliberately, so neither one alone is
enough. An existing admin mints a **single-use link**; the person who follows it
chooses their own username and password; and then an admin approves the account
before it can do anything.

The link's secret is never stored. `invites.json` holds `MAC(secret.key,
"invite" ‖ token)`, the same construction as the voter identifiers, so the file
cannot be turned back into working links. Lookup compares in constant time
against every stored digest, so timing does not reveal which invitations exist.

Claiming is a single operation under the store lock: it creates the account and
stamps the invitation as spent together, or does neither. That is what makes the
link genuinely one-time rather than one-time-if-nobody-races-you.

A claimed account is `Pending`. Its password works — being pending is an
authorisation state, not a broken credential — but `requireRole` sends it to a
waiting page and nowhere else. A pending admin is excluded from the admin count,
so an unapproved invitation can never be what lets the last real admin be
deleted.

## Drafts are previewable and takeable

An editor can open a draft and answer it exactly as a respondent would, because
checking the wording of a survey means using it, not reading it. `Accepting`
governs the public; `AcceptingFrom(now, previewer)` additionally lets a
previewer answer a draft. It does not relax anything else — a closed survey and
a passed close time still refuse a previewer.

Those answers are real records in the same log, which raises the obvious
question of what happens at publication. `FirstOpenedAt` settles it: the first
time a survey opens, its responses are discarded, because a survey that has
never been open can only contain an editor's test data. Every later state change
leaves responses alone, so closing and reopening a live survey is safe.

## Roles

| Role | Can |
|---|---|
| `viewer` | see results, download exports |
| `editor` | + create and edit surveys, moderate write-ins, open/close, delete |
| `admin` | + manage accounts |

Instance-wide, not per-survey. The last admin account cannot be deleted.

## Export

Two files, because they answer different questions.

- **responses.tsv** — one row per respondent, one `1`/`0` column per option,
  plus a selection count and the comment. This is the one you pivot.
- **summary.tsv** — one row per option: votes, respondents, share.

Percentages are the share of *respondents*, not of votes, and do not sum to 100:
options are not mutually exclusive. Tabs and control characters in free text are
replaced before writing, so a comment can never shift a column.

## Failure modes worth knowing

| If | Then |
|---|---|
| The process is killed mid-append | At most the last response line is lost; it is skipped at startup with a warning |
| `secret.key` is lost or changed | Every session and every voter identity is invalidated. Existing responses stay, but returning respondents look new and can vote again |
| Two processes share a data directory | Lost writes. There is no locking; don't |
| The data volume is not persistent | Everything is gone on restart, including accounts |
| A survey passes its `close_at` | It stops accepting responses; the state still reads `open`, and the UI says "closed by schedule" |
