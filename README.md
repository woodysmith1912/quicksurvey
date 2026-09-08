# QuickSurvey

A small self-hosted web app for "which of these are you interested in?" surveys.

You write a list of options, publish the survey, and hand out one URL. Anyone
with the link gives a thumbs-up to as many options as they like. Responses are
anonymous. Results download as TSV for Google Sheets.

- **Respondents** — no account, no sign-in, nothing identifying stored.
- **Accounts** — `viewer`, `editor`, `admin`, for reading results and editing surveys.
- **Storage** — one SQLite file in one directory. No database server to run.
- **Dependencies** — SQLite, via the pure-Go `modernc.org/sqlite`. No cgo.
- **Image** — distroless, non-root, read-only root filesystem. ~16 MB.

See [DESIGN.md](DESIGN.md) for how it works and what its failure modes are, and
[TODO.md](TODO.md) for what is and is not finished.

## Run it

```sh
make up                                           # docker compose up -d --build
docker compose logs quicksurvey | grep password   # the first admin password, printed once
```

Then open http://localhost:8080/, sign in as `admin`, and change the password —
the account cannot do anything else until you do.

Without Docker:

```sh
go build -o quicksurvey ./cmd/quicksurvey
./quicksurvey serve -data ./data -addr :8080 -secure-cookies=false
```

`-secure-cookies=false` is needed for plain HTTP on localhost, because a browser
drops a `Secure` cookie sent over `http://`.

## Configuration

All flags have an environment variable equivalent, which is what the container
uses.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-data` | `QS_DATA_DIR` | `/data` | Data directory. Must be a persistent volume. |
| `-addr` | `QS_ADDR` | `:8080` | Listen address. |
| `-base-url` | `QS_BASE_URL` | derived | External origin, e.g. `https://survey.example.com`. Used for the shareable link shown to editors. Derived from `Host` / `X-Forwarded-*` if unset. |
| `-tz` | `QS_TZ` | `Local` | Timezone for displaying and entering times, e.g. `America/New_York`. |
| `-secure-cookies` | `QS_SECURE_COOKIES` | `true` | Mark cookies `Secure`. Set false only for plain HTTP. |

## The container

`gcr.io/distroless/static-debian12:nonroot`. No shell, no package manager, no
libc — the only executable in the image is `quicksurvey` itself, which is also
what answers the Docker health check (`quicksurvey healthcheck`). The zone
database is compiled into the binary, so `QS_TZ` works with nothing installed.

It runs as uid 65532 with a read-only root filesystem and all capabilities
dropped. `/data` is the only writable path, and even temporary files are created
inside it rather than in `/tmp`.

### no-new-privileges

Not set by default, because it is incompatible with **snap-packaged Docker**.
The snap's daemon runs confined by AppArmor (profile `snap.docker.dockerd`), so
starting a container requires an AppArmor profile transition at `execve` — and
`no_new_privs` forbids profile transitions. Every container then exits
immediately with

```
exec /usr/local/bin/quicksurvey: operation not permitted
```

for any image, not just this one. Neither `--privileged` nor
`apparmor=unconfined` helps, because switching to "unconfined" is also a
transition.

Check whether your host can use it:

```sh
docker run --rm --security-opt no-new-privileges:true alpine echo ok
```

If that prints `ok`, turn it on:

```sh
QS_NNP=1 make up
# or: docker compose -f compose.yaml -f compose.no-new-privileges.yaml up -d
```

Little is lost without it: the image is distroless and contains no setuid or
setgid binary for a process to escalate through.

## Deployment

The container speaks plain HTTP and expects a reverse proxy in front of it doing
TLS. It honours `X-Forwarded-Proto` and `X-Forwarded-Host` when building links.
Caddy is the shortest path:

```
survey.example.com {
    reverse_proxy quicksurvey:8080
}
```

Two things the app deliberately does not do, and that your proxy should:

- **Rate limiting.** There is no throttle on sign-in attempts.
- **TLS.** Without it, `Secure` cookies do not work and nothing is private.

**One writer is still the design.** Run a single replica. Two processes sharing
a data directory will not corrupt it — SQLite serialises them — but nothing
above the storage layer expects a second instance, so there is no reason to run
one.

## Using it

1. Sign in and create a survey. It starts as a **draft** — the link 404s for
   everyone but you.
2. Edit the options. Open the draft yourself and **take it exactly as a
   respondent would**, write-ins and all, to check the wording. Those responses
   are test data and are discarded the first time you publish.
3. **Publish**.
4. Share the `/s/<id>` link.
5. Watch results on the survey's admin page. Approve, reword, merge or reject
   any options respondents suggested.
6. Close it by hand, or set a close time when editing.
7. Download the TSV.

### Per-survey settings

| Setting | Effect |
|---|---|
| Show the tally to respondents | Respondents see vote counts. Comments are never shown to them. |
| Let respondents suggest options | A write-in goes into a moderation queue. Only the person who suggested it can see it until it is approved; their vote for it counts from the moment it is. |
| Offer a comment box | One optional free-text field per response. Visible to accounts and in the export only. |
| Show the options in a different order to each respondent | **On by default.** Whichever option is listed first collects extra votes for being first; shuffling spreads that bias out. Each person keeps their own order, so returning to change an answer does not move the boxes around. Results and exports are unaffected. |
| Close automatically at | After this time the survey stops accepting responses. Leave blank to close it by hand. |

### Import into Google Sheets

Download either TSV, then in Sheets: **File → Import → Upload**, separator type
**Tab**.

- `…-responses-*.tsv` — one row per respondent, a `1`/`0` column per option, a
  selection count, and the comment. Pivot this.
- `…-summary-*.tsv` — one row per option: votes, respondents, share.

Shares are the percentage of *respondents* who picked an option. They do not add
up to 100%, because a respondent can pick any number of options.

## Adding people

Two ways, both under **Accounts** (admins only).

**By invitation link** — the usual way. Choose a suggested role and how long the
link stays valid, and you get a URL, shown once. Send it to them; they pick
their own username and password, so no password ever passes through you. The
link is **single use**: the first person to claim it spends it, and it stops
resolving for everyone else. Only a keyed digest of the link is written to disk,
so a leaked `invites.json` yields no working invitations.

A claimed account is **pending**. It can sign in and reach nothing but a page
saying it is waiting. An admin then approves it — choosing the role themselves,
which need not be the one the invitation suggested — or rejects it, which
deletes the account. A pending admin does not count towards the "you cannot
delete the last admin" rule.

**Directly** — type a username, password and role yourself. Useful for a first
colleague or for recovery.

## Anonymity

Nothing identifying is recorded with a response — no account, no IP address, no
user agent, no timestamp beyond when the response was written.

Repeat voting is deterred with a cookie. On first visit the browser is given a
random token. What is stored is a keyed hash of that token *and* the survey ID,
so the same browser looks like a different person on every survey, and no
response can be traced back to a browser even by someone holding the files.

This stops casual double voting — reloading, or clicking submit twice. It does
not stop a private window or a second browser, and is not intended to. Repeat
submissions replace the earlier answer rather than being rejected, so a misclick
is fixable and counts still cannot be inflated.

## Command line

Everything below is also doable in the web UI; the CLI exists for recovery and
scripting.

```sh
quicksurvey serve
quicksurvey user list
quicksurvey user add    -name alice -role editor     # password from stdin or $QS_PASSWORD
quicksurvey user passwd -name alice
quicksurvey user role   -name alice -role admin
quicksurvey user rm     -name alice
quicksurvey export -survey abc123 -kind responses > out.tsv
```

The CLI does not suppress terminal echo when prompting for a password. Pipe it
instead:

```sh
printf %s "$PW" | quicksurvey user add -name alice -role editor
```

These are safe to run against a live server: SQLite serialises the writes. In
Kubernetes, `kubectl exec` into the pod, or run a one-off pod mounting the same
volume — though with a `ReadWriteOnce` volume that has to land on the same node.

## Backup

```sh
quicksurvey backup -to /backups/quicksurvey-$(date +%F).db
```

Safe to run against a live instance. **Do not just copy `quicksurvey.db`** — in
WAL mode recent commits live in a side file, so a plain copy can catch the
database mid-write. `backup` uses SQLite's own `VACUUM INTO`, which produces a
consistent snapshot. The result is an ordinary SQLite file; restoring is copying
it back into the data directory.

In Kubernetes, prefer a scheduled CSI `VolumeSnapshot` — that happens at the
storage layer, so nothing has to mount the volume alongside the running pod.

The instance's HMAC key lives in the database, so a backup carries it. Losing it
invalidates every session and every voter cookie: existing responses survive,
but returning respondents look like new people and can vote again.

## Tests

```sh
make test         # 48 Go unit and handler tests
make e2e-docker   # 22 Playwright browser tests, in containers
```

`make e2e-docker` installs nothing on the host. It builds the image, starts a
throwaway instance on its own volume with a seeded administrator, runs
Playwright from the official image against it, and removes the volume
afterwards.

Every browser interaction is capped at 3 seconds and every test at 15. This
application does no work that can legitimately take longer — no external calls,
no queries, a map lookup and a template — so a slower response is a defect
rather than something to wait out. The whole suite runs in under 30 seconds.

`make e2e` runs the same specs against a locally built binary for a faster edit
loop, but needs `make e2e-install` first, which does install Node packages and
a browser on the host.
