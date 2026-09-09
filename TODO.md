# QuickSurvey — development TODO

<!-- Markers, for me:

     FIX   — do this next.
             - [ ] FIX  short description
             FIX: a paragraph explaining what is wanted
             I grep `^\s*(- \[[ x~]\] )?(FIX|NPTF)\b` before starting.

     NPTF  — No Plans To Fix. Acknowledged, understood, and deliberately
             accepted as it stands. Not a backlog item and not an oversight:
             a decision. I leave these alone, do not re-propose fixes for
             them, and do not count them as outstanding work. If one should
             be reopened, remove the marker.
-->

## Next
- [x] Deployed to DOKS at https://quicksurveys.plainwrapworks.com — real
      Let's Encrypt certificate, StatefulSet on a Retain volume, one replica.
- [x] Pushed to GitHub; CI publishes the image. The GHCR package inherits the
      repository's visibility, so it was already public — verified by pulling
      it anonymously.
- [x] Tagged v0.1.0. Note OCI tags drop the leading "v", so the manifests
      reference `0.1.0`.
- [x] DNS delegated to DigitalOcean and propagated; wildcard plus an explicit
      record, and mail records saying the domain sends and receives nothing.
- [ ] Traefik in this cluster stores `acme.json` inside its container with no
      volume, so it re-requests every certificate on restart. Not ours, but a
      Let's Encrypt rate-limit incident waiting to happen.

## Next
- [ ] FIX  `Tally` runs on every ballot GET when a survey shows results to
      respondents, walking every response row: 16.3ms and 2.7MB per request at
      1,000 respondents against 0.65ms without. It grows linearly with the
      survey. Cache the tally per survey and invalidate it on write, or
      aggregate in SQL rather than in Go. `BenchmarkBallot` measures it.

## Improvements
- [x] The front page is a splash that explains the site; the sign-in link sits
      quietly at the top right.
- [x] An administrator can open a draft survey and take it exactly as a
      respondent would. Those responses are test data and are discarded the
      first time the survey is published.
- [x] Administrators can be invited by a one-time link. The invitee creates
      their own account and lands in an approval queue, where an existing admin
      approves them with a role of the approver's choosing, or rejects them.

- [x] Options are shown in a different order to each respondent, on by default,
      stable per person so returning to edit an answer does not move the boxes.

## Done and verified by tests
- [x] `internal/store` — SQLite (`modernc.org/sqlite`, pure Go, no cgo);
      WAL, `BEGIN IMMEDIATE`, foreign keys, one connection
- [x] Accounts: PBKDF2-SHA256, viewer/editor/admin, last-admin protection
- [x] Survey and option model, soft delete, merge resolution through chains
- [x] One response row per respondent; resubmission replaces it in a transaction
- [x] Per-survey pseudonymous voter IDs; repeat submission replaces
- [x] Write-in moderation: pending is private to its author, counts on approval
- [x] TSV export, wide and summary; tabs and newlines in free text sanitised
- [x] HTTP layer: sessions bound to the password hash, CSRF, role gating,
      open-redirect protection, draft previews, scheduled close
- [x] Invitations: single-use links, digests only on disk, pending accounts
      gated to a waiting page until an admin approves them
- [x] `cmd/quicksurvey` — `serve`, `user`, `export`; bootstrap admin on first start
- [x] `quicksurvey backup` via `VACUUM INTO` — safe against a live instance
- [x] Dockerfile, compose file, Makefile
- [x] GitHub Actions: fmt, vet, `go test -race`, Playwright, manifest render,
      then publish to GHCR with a provenance attestation
- [x] Kubernetes manifests for DOKS (`deploy/k8s`): StatefulSet on a Retain
      block-storage volume, Traefik Ingress with automatic Let's Encrypt,
      app-level HTTPS redirect, nightly CSI VolumeSnapshots with retention
- [x] README.md, DESIGN.md
- [x] Go tests: 80 across `internal/store`, `internal/export`, `internal/web` — `make test`
- [x] Playwright specs: respondent, write-in/moderation, admin, accounts,
      splash, draft preview, invitations

## Verified on this machine
- [x] `make test` — 80 Go tests, green
- [x] `make e2e-docker` — 33 Playwright tests, green, 40s, nothing installed on
      the host. Slowest single test 1.8s; every interaction capped at 3s.
- [x] Image builds distroless and runs read-only as uid 65532; Docker reports
      the container healthy via `quicksurvey healthcheck`
- [x] `make up` / `make down`

## Not yet run
- [ ] `make e2e` (the host-installed variant) — the container path is the one
      that has actually been exercised. CI runs the host path on every push,
      so this is now covered there rather than locally.
## Later
- [ ] Configure outbound email, likely using Brevo free tier. The application has no need to send email, but the invitation and moderation flows
      would be more convenient if it did.

## Resolved during the build
- `docker compose up` failed on the development host with
  `exec /usr/local/bin/quicksurvey: operation not permitted`. Cause:
  snap-packaged Docker runs its daemon under AppArmor (`snap.docker.dockerd`),
  so each container start needs an AppArmor profile transition at `execve`,
  which `no_new_privs` forbids. Not image-specific — `docker run --security-opt
  no-new-privileges:true alpine echo hi` fails the same way. `no-new-privileges`
  is now off by default, with `compose.no-new-privileges.yaml` as an opt-in
  overlay for hosts where it works.
- Error messages rendered on the page that produced them were invisible:
  `setFlash` wrote a cookie but `render` read the flash off the incoming
  request, so a failed sign-in showed nothing and then leaked the message onto
  the next page. Split into `setFlash` (redirects) and `flashNow` (direct
  renders), with a regression test.

## Known gaps, deliberate
- Rate limiting is per-address and in-process, so it bounds abuse rather than
  preventing it. Sign-in bills only failures; voter identities are metered at
  issuance rather than at every vote, because a survey link shared inside one
  office puts a whole crowd behind one address.
- A determined person can still inflate a count by asking for new voter
  identities. The cookie deters accidents, not attackers, and the splash page,
  README and DESIGN.md now say so rather than implying otherwise.
- The operator can de-anonymise by joining the proxy's access log to response
  timestamps. Documented rather than papered over.
- One writer by design. SQLite makes a second process safe rather than
  corrupting, but nothing above the storage layer wants one — run one replica.
- The project no longer has zero dependencies. `modernc.org/sqlite` brings
  about ten modules. That was the price of not inventing a locking scheme, and
  it was worth paying; `CGO_ENABLED=0` and the distroless static image both
  survive.
- Superseded answers are not retained. Crash safety came from the append-only
  log; WAL provides it now, and keeping a history would mean storing more about
  respondents than the application needs.
- `quicksurvey user add` does not suppress terminal echo — that would cost an
  external dependency for a fallback path. Pipe the password instead.

## Deliberately not built (YAGNI)
- Question types other than thumbs-up. `Survey.Type` exists so adding one does
  not need a migration.
- Per-survey account permissions. Roles are instance-wide.
- Email, notifications, or any outbound network access.
