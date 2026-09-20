# Step 0 — surrogate keys for accounts

**Status: plan for review. Nothing here is built, and nothing has run against
the live database.**

This is step 0 of [MULTITENANT.md](MULTITENANT.md): give accounts an opaque
primary key, convert the five columns that currently hold usernames as
relationships into real foreign keys, and demote `users.name` to a unique,
mutable login-and-display attribute.

It ships on its own, before any group work, and is invisible to users apart
from one consequence noted below.

---

## Why now

Five columns across three tables are foreign keys in intent and plain text in
fact — no `REFERENCES`, no cascade, nothing:

| Column | Holds | Enforced |
|---|---|---|
| `resets.user_name` | whose password it resets | no |
| `resets.created_by` | which admin minted it | no |
| `invites.created_by` | which admin issued it | no |
| `invites.claimed_by` | who claimed it | no |
| `users.invited_by` | who invited them | no |

`DeleteUser` (`internal/store/users.go:265-289`) is a bare
`DELETE FROM users WHERE name = ?` with no cascade and nothing to cascade
with, so **this database already contains dangling references** wherever an
account has been removed. That is today's bug, not a hypothetical one.

The group work is what makes it urgent rather than merely wrong: `memberships`
would be the sixth such column and audit rows the seventh, and by then the
conversion touches membership and authorization data rather than three small
tables.

It also unblocks something the design wants later. Once nothing relates by
username, the username scheme can change — which is what the enumeration
oracle described in MULTITENANT.md eventually needs.

---

## Target shape

```sql
CREATE TABLE users (
  id                   TEXT PRIMARY KEY,
  name                 TEXT NOT NULL UNIQUE,   -- login identifier, now mutable
  role                 TEXT NOT NULL,
  hash                 TEXT NOT NULL,
  created              TEXT NOT NULL,
  must_change_password INTEGER NOT NULL DEFAULT 0,
  pending              INTEGER NOT NULL DEFAULT 0,
  invited_by_id        TEXT REFERENCES users(id) ON DELETE SET NULL,
  invited_by_name      TEXT NOT NULL DEFAULT '',   -- the name at the time
  sessions_from        TEXT
);

CREATE TABLE resets (
  id              TEXT PRIMARY KEY,
  digest          TEXT NOT NULL UNIQUE,
  user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_by_id   TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_by_name TEXT NOT NULL DEFAULT '',
  created         TEXT NOT NULL,
  expires         TEXT NOT NULL,
  used            TEXT
);

CREATE TABLE invites (
  id              TEXT PRIMARY KEY,
  digest          TEXT NOT NULL UNIQUE,
  role            TEXT NOT NULL,
  note            TEXT NOT NULL DEFAULT '',
  created_by_id   TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_by_name TEXT NOT NULL DEFAULT '',
  created         TEXT NOT NULL,
  expires         TEXT NOT NULL,
  claimed_by_id   TEXT REFERENCES users(id) ON DELETE SET NULL,
  claimed_by_name TEXT NOT NULL DEFAULT '',
  claimed         TEXT,
  revoked         INTEGER NOT NULL DEFAULT 0
);
```

Two decisions in there are worth arguing with.

**`resets.user_id` cascades; everything else sets null.** A reset link for a
deleted account is a dead single-use credential and should go with the account.
An invitation an admin issued is a record of something that happened, and
deleting the admin must not delete it.

**Every provenance column keeps the name beside the ID.** "Who invited this
person" has to stay answerable after the inviter is gone, and an ID that
resolves to a deleted row answers nothing. This is the same rule the audit
logging section of MULTITENANT.md states, applied to the tables rather than to
the log. If you would rather lose that history than carry the columns, say so
and they come out — but then account deletion starts erasing provenance.

---

## Backfilling, and the rows that will not map

`invited_by`, `created_by` and `claimed_by` are `NOT NULL DEFAULT ''`, so empty
strings are normal and map to NULL. The problem is the other kind: a name that
is not empty and does not resolve, left behind by a past account deletion.

**Quantify before deciding.** Against a copy of production, read-only:

```sql
SELECT 'invites.created_by', COUNT(*) FROM invites
  WHERE created_by <> '' AND created_by NOT IN (SELECT name FROM users)
UNION ALL SELECT 'invites.claimed_by', COUNT(*) FROM invites
  WHERE claimed_by <> '' AND claimed_by NOT IN (SELECT name FROM users)
UNION ALL SELECT 'users.invited_by', COUNT(*) FROM users
  WHERE invited_by <> '' AND invited_by NOT IN (SELECT name FROM users)
UNION ALL SELECT 'resets.user_name', COUNT(*) FROM resets
  WHERE user_name NOT IN (SELECT name FROM users);
```

Proposed handling, which the counts may change:

- **Provenance columns** — `*_id` NULL, `*_name` keeps the original string. The
  history survives; only the link is absent, which is honest, because the
  account it pointed at is gone.
- **`resets` rows with no matching account** — deleted. They are single-use
  credentials for accounts that no longer exist. Keeping them is strictly worse
  than dropping them, and the row count goes in the migration log.

---

## Doing it in SQLite

A primary key cannot be changed in place, so each of the three tables is
rebuilt: create the new shape, `INSERT … SELECT` through a join on `name`,
drop, rename, recreate indexes.

Three things about this database in particular:

**`foreign_keys` is on, set in the DSN** (`internal/store/store.go:105`), and
the rebuild has to turn it off around the drop-and-rename or the existing
`ON DELETE CASCADE` relationships will act on the intermediate states.

**The pragma is per-connection, and `database/sql` pools connections.** A
`PRAGMA foreign_keys = OFF` issued through the pool applies to whichever
connection happened to serve it and to nothing else, and the rebuild then runs
with the pragma on while appearing not to. The whole migration therefore runs
on a single `*sql.Conn` taken with `db.Conn(ctx)`. This is the detail most
likely to produce a migration that passes tests and misbehaves in production.

**`PRAGMA foreign_keys` is a no-op inside a transaction**, so the order is:
take the connection, pragma off, begin, rebuild, `PRAGMA foreign_key_check`
(must return zero rows, or roll back), commit, pragma on.

### Where it hooks in

`migrate()` (`internal/store/store.go:255`) applies `schema` then walks
`addedColumns`, and both are idempotent by inspection — `CREATE TABLE IF NOT
EXISTS`, and `ensureColumn` checking `pragma_table_info`. A table rebuild fits
that idiom (gate on whether `users` has an `id` column) but only just, and the
group work brings a second rebuild behind this one.

**Recommendation: introduce `meta['schema_version']` with this change.** The
`meta` table already exists and holds only the instance secret. Inspection-based
gating is fine for one rebuild and becomes guesswork at three.

### Sessions are invalidated

`SessionKey` is `Name + hash + SessionsFrom` (`internal/store/users.go:67-69`)
and the cookie body carries the username (`internal/web/auth.go:75-100`). Both
move to `users.id`, otherwise a rename — the thing this whole change is for —
logs the renamed person out and hands their cookie a name that resolves to
nobody.

**Consequence: every signed-in session ends when this deploys.** Everyone signs
in again. On an invitation-only instance with a handful of accounts that is a
minor annoyance, but it should not be a surprise, and it is the one
user-visible effect of the change.

---

## Code to change

| File | What |
|---|---|
| `internal/store/store.go` | `schema`, the new rebuild step, `meta['schema_version']` |
| `internal/store/users.go` | `userColumns:114`, `addUser`, `SetRole`, `DeleteUser`, `ApproveUser`, `SessionKey:67` |
| `internal/store/reset.go` | `resetColumns:43`, the two `WHERE user_name` queries at `:85` and `:187` |
| `internal/store/invite.go` | `inviteColumns:55`, `claimed_by` at `:167` and `:223` |
| `internal/web/auth.go` | session cookie body and lookup, `:75-100` |
| `internal/web/admin.go` | the accounts page and the reset-link path at `:396-403` |
| `internal/web/invite.go` | the `invited_by` log line at `:61` |

`User.InvitedBy`, `Invite.CreatedBy`, `Invite.ClaimedBy` and `Reset.CreatedBy`
are JSON-tagged and reach the templates, so the accounts and invitations pages
render from the `*_name` columns rather than from the IDs.

---

## Testing

`migrate_test.go` already has the right shape: build a database the way the
previous release left it, `Open()` it, and assert the data is still there. That
test exists because a release shipped that could not read its own users table,
and this change is a much larger version of the same risk.

Add `schemaV2` — the current shape — populated with:

- an account that signs in afterwards, password unchanged;
- an outstanding reset link that still resolves to its account;
- an invitation whose creator and claimer still render;
- **a dangling `invited_by` naming an account that is not there**, asserting the
  chosen handling rather than a crash;
- an empty-string `created_by`, asserting it becomes NULL and not a broken FK.

Then assert `PRAGMA foreign_key_check` returns nothing, and that a second
`Open()` is a no-op rather than a second migration.

---

## Rehearsal and rollback

**Rehearsal.** `quicksurvey backup -to -` streams a consistent copy out of the
distroless container (`deploy/k8s/README.md:156`):

```
kubectl -n quicksurvey exec quicksurvey-0 -- quicksurvey backup -to - > prod-copy.db
```

Run the new binary against that copy locally. Compare row counts per table
before and after, run the dangling-reference queries above again expecting
zero, and confirm `foreign_key_check` is clean. Only then does it go near the
cluster.

**Rollback is a data restore, not a redeploy.** The migration runs on `Open()`,
so once it has run, the previous image cannot read the database — its queries
name columns that no longer exist. Rolling back means restoring the volume
snapshot and losing everything written since. That makes the deploy effectively
one-way, which is the argument for doing it while the instance is small and
before groups multiply what is at stake.

A fresh snapshot is taken immediately before the deploy rather than relying on
the 03:17 nightly.
