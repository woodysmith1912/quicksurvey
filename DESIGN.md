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
| Go, single container | One static binary; templates and CSS embedded with `embed.FS`, zone database compiled in with `time/tzdata`. `CGO_ENABLED=0` throughout, which is why the SQLite driver is `modernc.org/sqlite` rather than the cgo one |
| No database | State is files under one data directory, mirrored in memory |
| Responses anonymous | Nothing identifying is stored — no account link, no IP, no user agent. The proxy in front still logs one; see below |
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
- **Getting anything out.** `quicksurvey initial-password` prints the bootstrap
  credential because there is no `cat`; `quicksurvey backup -to -` streams a
  snapshot to stdout because there is no `tar` and so `kubectl cp` cannot work.

The rule this keeps producing: if an operator needs to do something inside the
container, the binary has to do it. Documentation that reaches for a shell is
documentation of a command that cannot run.

`/data` is created in the image owned by 65532, so a fresh named volume
inherits that ownership rather than being created root-owned and unwritable.

`no-new-privileges` is deliberately off. Under snap-packaged Docker the daemon
runs confined by AppArmor, so every container start needs a profile transition
at `execve`, which `no_new_privs` forbids — containers die immediately, for any
image. The flag is available as an opt-in overlay for hosts where it works, and
costs little when absent: there is no setuid binary in the image to escalate
through.

## Storage

Everything lives in one SQLite database:

```
$QS_DATA_DIR/quicksurvey.db      accounts, invitations, surveys, options,
                                 responses, and the instance HMAC key
```

The first version of this used hand-rolled files — JSON definitions rewritten
atomically, responses in an append-only log, replayed at startup. That was
defensible for one container on one host. It stopped being defensible once the
target became Kubernetes, where the platform will start a second pod on your
behalf during a rollout. Two writers against those files interleave and corrupt
silently. The alternative on offer was a `flock` scheme of my own invention
guarding a file format of my own invention; SQLite is the same guarantee from
something exercised by billions of installations.

It also deleted code. `AddWriteIn` used to create an option, write the survey,
append a response, and hand-roll an undo if the second write failed.
`ClaimInvite` had the same shape. Those were transactions written out longhand;
`BEGIN`/`COMMIT` replaced them and the bugs hiding in them.

The original constraint survives: SQLite is a library, not a service. Still one
local file, still no daemon to run, patch or secure.

### Connection settings, and the one that matters

```
_pragma=journal_mode(WAL)      readers do not block the writer
_pragma=foreign_keys(1)        the ON DELETE CASCADEs actually fire
_pragma=synchronous(NORMAL)    a crash can lose the last transaction, never the file
_pragma=busy_timeout(10000)    a waiting writer waits instead of failing
_txlock=immediate              see below
```

`_txlock=immediate` is the one that is easy to get wrong, and the concurrency
test caught it being wrong. A plain `BEGIN` is *deferred*: the transaction opens
as a reader and tries to become a writer at its first write. If another
connection committed in between, that upgrade fails with `SQLITE_BUSY_SNAPSHOT`
— and `busy_timeout` cannot rescue it, because no amount of waiting makes the
upgrade legal; the transaction has to be discarded and retried. Taking the write
lock up front with `BEGIN IMMEDIATE` turns an unrecoverable error into an
ordinary wait.

The pool holds four connections.

One was the original choice, reasoning that writes serialise anyway. That was
wrong: it serialised *reads* too, discarding the concurrent readers WAL provides
for free. `BenchmarkBallot` put a number on it — 611µs to serve a ballot at one
connection, 272µs at four, a 2.2x difference for a one-line change.

Four rather than more because sixteen measured *worse* than four, at 328µs. Past
the point where readers overlap, extra connections buy nothing and cost
scheduling. Writes still serialise, which is correct, and `_txlock=immediate`
with `busy_timeout` is what makes that a wait rather than an error.

### What the benchmark actually found

| | fresh ballot | ~10 selected | +1,000 other respondents | + results shown |
|---|---|---|---|---|
| 1 connection | 611µs | 651µs | 653µs | 16.3ms |
| 4 connections | 272µs | 292µs | 283µs | 4.85ms |

Loading a respondent's existing answer costs about 40µs — the response and its
choices are two indexed lookups, and the number of prior respondents does not
enter into them.

Showing results to respondents used to cost 25x, because `Tally` walked every
response on every ballot load. It is now cached, keyed on a counter that every
committed write bumps, which took that case from 4,851µs to 359µs. See below.

### At realistic scale

Against a seeded database of 10,000 surveys, 99,853 options, 75,173 responses
and 250,790 choices — 50.8MB:

| | ns/op | requests/s |
|---|---|---|
| fresh ballot | 215µs | ~4,650 |
| returning respondent | 258µs | ~3,870 |
| with results shown | 263µs | ~3,800 |

The database being large does not matter. Those numbers are *better* than the
synthetic benchmark's, because the seeded surveys carry seven to fifteen options
against its thirty: every lookup is indexed, so the cost is the size of the
survey being read, not how many surveys sit beside it.

Building that fixture through the ordinary store API — one transaction per
survey, one per response — took 45 seconds, and stayed linear throughout: 4.18s
for the first thousand surveys, 4.51s for the last.

**SQLite's locking is unreliable on NFS.** On a block device — which is what a
DigitalOcean Block Storage volume is — it behaves correctly. An `RWX`
NFS-backed volume would not be safe, and neither would a lock file.

### One row per respondent

`responses` holds one row per (survey, voter), replaced on resubmission, with
`choices` as a child table. Superseded answers are not retained. The append-only
log existed for crash safety, which WAL now provides; an audit trail was never
asked for, and keeping one would mean storing more about respondents than the
application needs — which cuts against the point.

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
private window, a second browser, a cleared cookie jar, or a client that simply
invents a cookie value — `voterToken` accepts any token the browser presents.
Anything stronger needs identity, which the requirement rules out.

Two limits worth stating plainly, because the property being sold here is
anonymity and a guarantee that does not hold is worse than no guarantee:

- **Counts can be inflated by a determined person.** Minting fresh cookie values
  costs nothing, and there is no rate limit in the application. The cookie
  deters accidents, not attackers.
- **The operator can usually de-anonymise.** The application stores no IP, but
  the reverse proxy logs one alongside a request time, and responses carry a
  timestamp. At the scale these surveys run at, lining the two up is easy for
  anyone holding both. The honest claim is that the application does not collect
  identities, not that correlation is impossible.

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

## Option order

Respondents see the options shuffled, by default. Position bias is real — the
option listed first collects votes for being first — and a survey meant to
measure interest should not also measure list position.

The shuffle is derived from the respondent's own per-survey identifier, not
drawn fresh on each render. That is the part that matters: someone who reloads,
or comes back to change their answer, must see the same order, or their existing
ticks appear to move. Different people get different orders; each person keeps
theirs for the life of their cookie.

It is strictly a presentation concern. `Ballot` returns the editor's order and
is what the admin pages, the tally and the exports use; `BallotFor` is the
shuffled view and is used by exactly one caller, the respondent's page.

The setting is stored inverted, as `NoRandomize`, so that the zero value means
"shuffle". That makes the default apply to surveys written before the field
existed, without a migration.

## Authentication

Passwords are PBKDF2-HMAC-SHA256, 600,000 iterations, per-user salt, stored in a
self-describing string so the cost can be raised later without invalidating
accounts. Unknown usernames still pay for one verification, so response time
does not enumerate accounts.

Sessions are stateless signed cookies: `name|expiry|HMAC(secret, name|expiry,
session_key)`, where the session key covers the user's password hash and a
`sessions_from` instant. Changing a password invalidates every cookie, and so
does signing out — which is what gives "Sign out" something to do, since
clearing a cookie does nothing about a copy captured beforehand. There is no
session table, so revocation is per account rather than per device: signing out
signs out everywhere. For an administrative account that is the safer default.
A restart signs nobody out, because the key is on disk.

CSRF tokens are derived from whichever cookie identifies the caller — the
session for accounts, the voter token for respondents. Both cookies are
`HttpOnly`, so a cross-site page cannot read the token it would need to forge a
request.

## The tally cache

`Tally` is called on every ballot load of a survey that shows results to
respondents. Computing it walks every response and every choice, so it grew
linearly with the survey while nothing else did: 16ms and 2.7MB per request at
a thousand respondents, against 0.65ms and 123KB for the same page without.

It is cached now, and the interesting part is the invalidation. Every write that
can change a tally — a response, a moderation decision, a merge, publishing,
deleting a survey — goes through `Store.tx`, so that is where a counter is
bumped, and the cache is keyed on it. Nothing has to remember to invalidate at
seven call sites, and a new write path cannot be added that quietly serves stale
counts.

The counter is global rather than per survey, so any write invalidates every
cached tally. That is deliberate: on the surveys where this matters, ballot
loads vastly outnumber votes, so the hit rate stays high, and correctness does
not depend on tracking which survey a transaction touched.

A cache that can serve a stale count is worse than no cache, because the count
is the entire product. `TestTallyCacheIsInvalidatedByEveryWritePath` walks each
of those paths and insists the next read reflects it, and a concurrent test
hammers reads against writes under `-race`.

## Migrations

`CREATE TABLE IF NOT EXISTS` does nothing at all to a table that already exists,
including adding a column to it. A column added in a later version therefore
never appears in a database created by an earlier one, every `SELECT` naming it
fails, and — because a failed query returned nil — the application reported *no
accounts* rather than an error.

That shipped. 0.2.0 could not read its own users table on an existing database,
so nobody could sign in and nothing said why. Respondents were unaffected, since
voting never reads accounts, which is what made it quiet.

Two changes came out of it. Columns added after a table first shipped are listed
in `addedColumns` and applied with `ALTER TABLE ... ADD COLUMN` when missing, so
they belong in that list as well as in the schema text. And a query that fails
is now logged at ERROR rather than returning nil silently: a failed query is not
the same as one matching nothing, and the difference has to be audible.

`TestUpgradeFromAnEarlierSchema` builds a database with the previous release's
schema and opens it, and `TestEveryQueriedColumnExistsAfterMigration` runs every
query the application issues. Both fail if a future column is added to the
schema text and not to `addedColumns`.

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

## Password resets

An administrator can hand out a reset link. They cannot set a password, and
cannot create an account with one either — the two are the same power wearing
different hats, since both end with someone else knowing a credential that signs
in as you.

The distinction matters more than it looks: an admin who sets a password can
then sign in as that person, which is a different and larger power than managing
accounts. Handing over a link keeps the two apart — the owner picks the secret
and the admin never learns it.

Mechanically it is the invitation flow again: 24 bytes of randomness in the URL,
only `MAC(secret, "reset" ‖ token)` on disk, constant-time lookup, and the
password change and the spending of the link in one transaction so a single link
cannot set two passwords. Issuing a new link retires any outstanding one, so an
account never has two live ways in. Using a link revokes that account's sessions,
since a reset is usually a response to something going wrong.

Two differences from invitations. The window is two hours rather than a week,
because a reset is handed over during a conversation and used immediately. And
the link is rendered directly from the request that created it rather than
redirected to with the token in a query string, which would put the secret into
browser history and the proxy's access log.

The CLI can still set a password directly. That needs a shell inside the
container, which is a different threat model, and it is the break-glass path
when nobody can sign in at all.

### Turning the limits off

`-login-rate=-1` and `-voter-rate=-1` disable them. That exists for load
testing: a benchmark run from one address is indistinguishable from an attack,
and measuring the limiter rather than the application is not the point of the
exercise.

Zero means "unset" and falls back to the default, so forgetting to configure a
limit cannot silently remove it — disabling has to be asked for with a negative
number. A disabled limiter is a nil pointer whose methods are nil-safe, so there
is no way to half-disable one, and start-up logs `RATE LIMITING IS OFF` at WARN
so a run left that way is visible in the first line of its log.

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
| The process is killed mid-write | WAL rolls back the incomplete transaction on the next open; committed responses survive |
| The instance key is lost | Every session and every voter identity is invalidated. Existing responses stay, but returning respondents look new and can vote again |
| Two processes share a data directory | Both work. SQLite serialises them; `BEGIN IMMEDIATE` plus `busy_timeout` turns contention into waiting. Nothing above the storage layer wants a second instance, so still run one |
| The data volume is not persistent | Everything is gone on restart, including accounts |
| A survey passes its `close_at` | It stops accepting responses; the state still reads `open`, and the UI says "closed by schedule" |
| You copy `quicksurvey.db` by hand from a running instance | You may get a torn snapshot. Use `quicksurvey backup`, or a storage-layer snapshot |
