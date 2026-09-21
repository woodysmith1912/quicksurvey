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

## Next
- [x] `Tally` is cached, invalidated by a write counter bumped in Store.tx.
      4,851µs -> 359µs at 1,000 respondents with results shown.

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
- [ ] Purge outdated images from the registry. The CI workflow does not yet do this, and the GHCR free tier has a 10GB limit.
- [ ] Improve backups from snapshots to dump/restic to S3
- [ ] add observability

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

## Restore hardening — predates the shown-count work, found reviewing it

These are all reachable on `main` today, independently of the `seen` feature.
Five reviewers went over that branch and kept finding them; they belong here
rather than in that branch's history, because fixing them there would have
hidden that they are already shipped.

- [x] An oversized restore is an unauthenticated, persistent OOM. Fixed: the
      option ceiling moved to `saveSurvey`, which every write passes, so the
      survey that caused it can no longer be created by any route. `MaxUpload`
      dropped to 1 MiB and `GOMEMLIMIT` set below the container limit.
      Was: `RestoreSurvey` enforced no ceiling on option count, and it restores
      `state`, so an editor can upload an 8 MiB "backup" carrying ~345,000
      options that comes back **open**. `GET /s/{id}` is unauthenticated and
      `render` buffers the whole page: one anonymous request measured a 146 MiB
      body, 858 MiB peak heap and ~1 GB RSS against a 192Mi pod. The survey
      persists, so the pod is killed again on every subsequent request by
      anyone who has the URL. The upload needs the editor role, but the payload
      is a plausible-looking JSON file that no editor can eyeball.
      The ceiling belongs where every write passes rather than in `AddOption`
      alone — see the next item, which is the same defect from the other side.

- [x] `maxOptions` is enforced on one of three write paths, so it is not an
      invariant. Fixed: it is checked in `saveSurvey` now, with a carve-out so
      a survey already over the ceiling can be saved unchanged or smaller and
      edited back down rather than frozen.
      Was: only `AddOption` checked it; `CreateSurvey` and `RestoreSurvey` did not.
      A survey past 500 options is therefore constructible through the ordinary
      new-survey textarea, and once it exists every write-in is refused forever
      with "survey already has the maximum of 500 options" — true, and useless
      to the anonymous respondent who sees it. Enforce it in `saveSurvey`,
      which every write passes, with a carve-out so an over-size survey can
      still be edited downwards rather than being frozen.

- [x] The 8 MiB upload cap is too large for a 192Mi container, and the runtime
      does not know the limit exists. Fixed: `MaxUpload` is 1 MiB and
      `GOMEMLIMIT` is 160MiB in the StatefulSet, so the collector works against
      the cgroup rather than discovering it by being killed.
      Was: decoding an adversarial 8 MiB document peaked at 190.7 MiB in a
      standalone process — 99.3% of the cap, before any serving footprint. The
      cap was sized on "far past anything this application is for", which is
      true of real backups and irrelevant to hostile ones. Lower it, and set
      `GOMEMLIMIT` in the StatefulSet so the GC works against the limit instead
      of being OOM-killed blind.

- [ ] FIX  Restoring a large backup can exceed the container limit on its own.
      FIX: a 5000-response, 300-option backup peaked at 272-312 MiB. Measured
      in both trees, so this is not new. Streaming the responses rather than
      materialising the whole document would fix it; so would a smaller upload
      cap.

- [ ] FIX  A restore holds the write lock for the whole document.
      FIX: `saveSurvey` issues one `Exec` per option inside a single
      `BEGIN IMMEDIATE`. A very large survey measured 14-20s, against a 10s
      `busy_timeout` — so every concurrent vote fails for the duration.

- [ ] FIX  Duplicate references in an uploaded document surface raw SQLite text.
      FIX: `choices` is not deduplicated on restore, so a document repeating a
      choice fails with `UNIQUE constraint failed: choices.response_id,
      choices.option_id` flashed verbatim to the operator. Nothing is partially
      written. Two option entries sharing an id collapse silently instead, via
      the upsert in `saveSurvey`. Neither is validated.

- [ ] FIX  `MaxOptionText` is not enforced on restore.
      FIX: `AddOption` caps option text at 200 characters because it is
      re-rendered on every ballot and becomes an export column header. The
      restore path only trims.

- [ ] FIX  `Store.Responses` swallows read errors.
      FIX: `attach` logs and returns on a failed query, leaving partially
      populated responses. Since `seen`, a truncated read renders as "never
      shown" rather than as an error, and `Tally` caches it until the next
      write. Propagate the error, or refuse to cache a tally whose read failed.

## Performance, measured and accepted for now

Recorded rather than fixed: no high-load use is expected soon. Figures are in
the branch's review history, not here.

- [ ] NPTF The cold tally costs about three times what it did before `seen`.
      `countOnce` resolves each option id through a linear scan of the option
      list, the same shape that was fixed in the exporter. A resolve map there
      measured a useful improvement and is the obvious first move if this ever
      matters. Most of the remainder is the row volume `seen` adds, which
      counting in SQL rather than in Go would remove.
- [ ] NPTF The wide export carries a flat overhead per response, from the two
      maps `resolvedSets` builds for each one. Marking a reusable array indexed
      by column measured at parity with the pre-feature exporter.
- [ ] NPTF The `seen` backfill chunks by whole survey, so a single large survey
      still runs as one unbounded transaction with no resume point. Chunking by
      response id range within a survey would bound every shape.
- [ ] NPTF Reading a survey document parses it twice. `ReadSurveyDocument`
      unmarshals the whole body leniently just to read `format` and `version`,
      then decodes it again strictly. On a large backup the header scan is
      about half the parse phase and all of it is wasted work; the version
      could be read from the leading tokens instead. It exists so that a
      document from a newer build reports its version rather than blaming a
      field it does not recognise, which is worth keeping — just not at this
      price if restores ever get big.
- [ ] NPTF `Survey.Option` is a linear scan returning a large struct by value.
      It is the shared root of the tally and export costs above.

## Deliberately not built (YAGNI)
- Question types other than thumbs-up. `Survey.Type` exists so adding one does
  not need a migration.
- Per-survey account permissions. Roles are instance-wide.
- Email, notifications, or any outbound network access.
