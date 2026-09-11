# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

QuickSurvey: a single-binary Go web app for anonymous "thumbs-up as many as you like" surveys. One SQLite file, no cgo, distroless image, meant to run as one replica behind a TLS-terminating proxy. `README.md` is the operator manual; `DESIGN.md` explains *why* things are the way they are and records defects already found and fixed. Read `DESIGN.md` before changing storage, auth, the tally, or option/response semantics. `TODO.md` uses `FIX` / `NPTF` markers; `NPTF` items are deliberate decisions, not backlog. `MULTITENANT.md` is an unbuilt design draft.

## Commands

```sh
make check                 # gofmt -l -w . && go vet ./... && go test ./...
make test                  # go test ./...
go test -race -count=1 ./...                       # what CI runs
go test ./internal/store -run TestName             # one test
go test ./internal/web -run 'BenchmarkBallot' -bench . -benchmem
make build                 # go build -trimpath -o quicksurvey ./cmd/quicksurvey
make run                   # serve on :8080 with -secure-cookies=false and ./data
make up / make down        # docker compose (QS_NNP=1 adds no-new-privileges overlay)

make e2e-docker            # Playwright in containers; installs nothing on host
make e2e-install           # once: npm install + chromium on host
make e2e                   # Playwright against a locally built binary (faster loop)
cd e2e && npx playwright test tests/admin.spec.ts   # one spec (needs make build first)
```

CI gates on `gofmt -l .` being empty, `go vet`, `go test -race`, the Playwright suite, and `kubectl kustomize deploy/k8s` rendering with the Ingress host matching `QS_BASE_URL`. `CGO_ENABLED=0` throughout; the SQLite driver is `modernc.org/sqlite` for that reason.

## Architecture

Three packages plus a CLI:

- `cmd/quicksurvey` — subcommands `serve`, `user …`, `export`, `healthcheck`, `backup`, `initial-password`. Every flag has a `QS_*` env equivalent. The image is distroless (no shell, no `cat`, no `tar`), so anything an operator needs inside the container must be a subcommand of the binary. `time/tzdata` is compiled in.
- `internal/store` — the only persistence layer. `Open(dir)` creates `quicksurvey.db`, applies the idempotent schema, then `addedColumns` via `ALTER TABLE`. **Any column added to an existing table must also go in `addedColumns`**; `TestUpgradeFromAnEarlierSchema` and `TestEveryQueriedColumnExistsAfterMigration` enforce this. DSN uses WAL, `foreign_keys`, `busy_timeout`, and `_txlock=immediate` (deferred `BEGIN` fails with `SQLITE_BUSY_SNAPSHOT` under contention). Pool is `MaxConns = 4`, measured.
- `internal/web` — one `Server` handling both the anonymous `/s/{id}` pages and authenticated `/admin/` pages. Templates and CSS are `embed.FS`; every page is parsed with `base.html` plus any `_*.html` fragment. `requireRole` gates admin routes by `viewer < editor < admin` and diverts pending accounts to a waiting page. Two rate limiters: failed logins and *new voter identity issuance* (not votes).
- `internal/export` — TSV writers (responses wide format, summary per option).

### Invariants to preserve

- **All writes go through `Store.tx`.** It bumps a global generation counter that invalidates the tally cache. A write path that bypasses it serves stale counts; `TestTallyCacheIsInvalidatedByEveryWritePath` checks each path.
- **Options are never deleted.** Status transitions instead: `approved`, `pending`, `rejected`, `removed`, `merged`. `Resolve` follows merge pointers (bounded, so cycles resolve to "not counted"). Responses reference option IDs forever.
- **A resubmission only touches options the respondent could see.** Choices for removed/merged options are carried forward untouched. Taking the form literally lost votes; see `option_edit_test.go`.
- **`seen` records what was on the ballot at submit, unioned across submissions.** Visibility has one definition, `visibleTo` in `response.go`; use it rather than re-deriving approved-plus-own-pending. For every approved option `Votes <= Shown <= respondents`.
- **`FirstOpenedAt` is stamped in `saveSurvey`, not `Publish`.** The first open discards a draft's preview responses; every later state change leaves responses alone.
- **Anonymity:** voter ID is `HMAC(key, "voter"‖survey‖token)[:22]`. Never store the raw cookie token, an IP, or a user agent with a response.
- **Secrets never hit disk in the clear.** Invite and reset links store only `HMAC(key, prefix‖token)`; lookup is constant-time across all digests. Claiming/using a link and its side effect happen in one transaction. Setting a password by any route retires outstanding reset links.
- **Sessions are stateless cookies** keyed on the password hash plus `sessions_from`. Password change or sign-out invalidates every device for that account. CSRF tokens derive from the identifying cookie (session or voter).
- **Rate limit config:** `0` means "use default", negative disables (logs `RATE LIMITING IS OFF`). A disabled limiter is a nil pointer with nil-safe methods.
- **`Ballot` returns editor order; `BallotFor` is the per-respondent shuffle** and has exactly one caller (the respondent page). Randomize is stored inverted as `NoRandomize` so the zero value shuffles.
- **Close times** are wall-clock text parsed with `ParseInLocation` in `cfg.Location`, stored as UTC; comparison is absolute. DST was checked and is fine.

### Testing conventions

- Go tests build a real store in `t.TempDir()`; web tests use `newHarness(t)` and a cookie-jar `browser` with redirects disabled so `Location` can be asserted. Tests fetch the CSRF token from the rendered page rather than bypassing it.
- Most past defects were *orderings* of individually correct features, not uncovered lines. `internal/store/ordering_test.go` and `option_edit_test.go` enumerate state-machine interleavings; add to them when touching survey/option/response/account/link transitions.
- Playwright: single worker, shared state on purpose, 3 s per action, 15 s per test. A slower page is a defect, not a reason to raise the timeout. Selectors use `data-testid`. `e2e/helpers.ts` creates accounts only via invite-and-claim, because that is the only way the UI offers.
- Concurrency tests run under `-race`; two-writer behaviour on one database is where a data race would hide.

## Deployment notes that affect code

- Honour `X-Forwarded-Proto` / `X-Forwarded-Host` for link building; `TrustProxy` governs which address the rate limiter sees. `/healthz` is exempt from the HTTPS redirect because kubelet probes over plain HTTP.
- `Secure` cookies default on; `-secure-cookies=false` is required for plain-HTTP localhost.
- Only `/data` is writable in the container. Temp files go there, not `/tmp`.
- Kubernetes manifests in `deploy/k8s` pin `sha-` image tags; `latest` moves only on a `v*` tag.
