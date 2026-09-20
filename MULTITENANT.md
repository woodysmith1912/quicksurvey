# Groups and multi-tenancy — design

**Status: draft for review. Nothing here is built.**

Revised to the group model: *groups* rather than tenants, an implicit personal
group behind every account, a six-role ladder across two scopes, surrogate keys
for every relationship, and opaque group IDs in URLs.

Decided so far: Q1, Q3, Q6, Q7, Q9, Q10, and the identity question below.
Still open: Q2, Q4, Q5. Q8 is largely withdrawn — opaque IDs removed the
problem it existed to solve. Numbers are kept as they were so the discussion
stays anchored; the gaps are deliberate.

Each open question carries a recommendation. Answer inline — write under the
question, or strike the recommendation and put your own. I will treat what is
in the file as the decision and update the rest of the document to match before
writing code.

---

## What this changes

Today the application has no ownership at all. `Surveys()` returns every survey
in the database, and anyone who can reach `/admin/` sees all of them. There is
one instance-wide role per account and that is the whole authorization model.

Groups introduce the first authorization boundary the application has ever had.
That is a bigger change than adding a column: it means every read has to answer
"on whose behalf?", and the failure mode of getting it wrong is one customer
reading another's results.

---

## Identity: nothing relates by a user-visible string

**Decided.** Every relationship in the schema keys on an opaque surrogate ID.
No table refers to an account by its username, and no table refers to a group
by its name.

The current schema does the opposite, and does it without even the safety net
of a constraint. `users.name` is the primary key, and five columns across three
tables carry usernames as relationships with **no `REFERENCES` clause at all**:

| Column | Holds | Enforced |
|---|---|---|
| `resets.user_name` | whose password it resets | no |
| `resets.created_by` | which admin minted it | no |
| `invites.created_by` | which admin issued it | no |
| `invites.claimed_by` | who claimed it | no |
| `users.invited_by` | who invited them | no |

These are foreign keys in intent and plain text in fact. `DeleteUser`
(`internal/store/users.go:265-289`) is a bare `DELETE FROM users WHERE name = ?`
with no cascade and nothing to cascade *with*, so deleting an account leaves
every one of those rows pointing at a name that no longer exists. Rename an
account — something nothing supports today precisely because of this — and they
would point at the wrong person instead, which is the worse failure.

So:

```sql
ALTER TABLE users ADD COLUMN id TEXT;   -- backfilled, then PRIMARY KEY
```

`users.id` is opaque and random, from the same generator that produces survey
IDs. `users.name` stays, becomes `UNIQUE NOT NULL`, and demotes to what it
always should have been: the login identifier and display string, mutable
without touching a single relationship.

Every column in the table above converts to `*_id` with a real
`REFERENCES users(id)` and an explicit `ON DELETE` rule. Every new table in
this document keys on `users.id` from the start.

**This is a change to the live schema, not only to the design.** It is written
up here because the group work is what makes it urgent — `memberships` would
otherwise add a sixth unenforced username column, and audit rows a seventh —
but the conversion of the five existing columns stands on its own and is
described separately under [Sequencing](#sequencing) as step 0.

Out of scope: `responses.voter` holds an anonymous per-respondent cookie token
(`qs_voter`, `internal/web/auth.go:20`), not an account. It stays as it is.

## The model

```
group        a collection of users and surveys
survey       belongs to exactly one group, permanently
user         one account, site-wide
membership   (user, group, role) — a user may have many
```

Every account also has a **personal group**, created in the same transaction as
the account and owned by it. It is an ordinary group in every respect the code
cares about — same table, same membership rows, same authorization path — with
two special properties: nothing creates it explicitly, and it cannot be deleted
while the account exists.

A user works within one group at a time. A survey never moves between groups;
see [What I am not proposing](#what-i-am-not-proposing), which also covers what
that costs.

---

## Names, IDs, and enumeration

**Decided: group names are not unique, and never appear in a URL.**

A unique name is an enumeration oracle. If creating a group called `acme` can
fail because that name is taken, then anyone who can create a group can test
whether Acme is a customer of this instance. For an application whose pitch is
discretion — the same reasoning that keeps the group out of respondent URLs —
that is a customer list handed out for free.

The leak is not uniqueness itself. It is the **taken/not-taken answer**. So:

- `groups.name` is display-only: free text, not unique, not indexed for lookup.
- `groups.id` is opaque and random, and is what appears in URLs.
- All groups share one prefix, personal and ordinary alike: `/g/{id}/admin/...`.
  A personal group is not distinguishable from any other by its URL.

**Q8 is withdrawn.** Reserved words, the slug regex, and the username/slug
collision problem were all downstream of putting a human-chosen string in the
path. Nothing chooses a path component any more.

### The same oracle exists today, for usernames

Usernames must stay unique, because they are how people log in. That
reintroduces exactly the oracle described above, and it is live right now:
`addUser` returns `user "alice": already exists` (`internal/store/users.go:184`)
and `handleInviteClaim` hands store errors straight back to whoever is claiming
the invite (`internal/web/invite.go:46-56`). An invitee types a name and learns
whether that account exists.

It is worth noticing that the codebase already closed this hole at the other
door: `Authenticate` verifies a dummy password for unknown usernames
specifically "so response time does not reveal which accounts exist"
(`internal/store/users.go:291-298`). The invite form undoes at the front what
login is careful about at the back. Today that costs little — one tenant,
everyone is a colleague. With groups it is precisely the cross-group leak the
rest of this document exists to prevent.

**Not fixed here, but unblocked by it.** The fix is not to stop letting people
choose a name; it is to never report a collision — append a random
discriminator unconditionally, so `woody` becomes `woody-4f7a` whether or not
anyone else picked `woody`. (The near-miss version, silently falling back to
`woody2`, still leaks: getting `woody2` tells you `woody` exists.) That is a
separate change with its own user-visible consequences, and the surrogate-key
work above is what makes it possible to make later instead of never — once
nothing relates by username, changing the username scheme touches one column.

Worth knowing before considering alternatives: this codebase has **no email
at all** — no column, no SMTP, nothing, which is why reset links are handed out
by hand. "Log in with your email address" is not a tweak, it is a subsystem.

---

## Roles

The single `users.role` column cannot express this any more, because "admin"
now means two different things. There are two independent ladders, and both are
ranked, so both are expressible with the `AtLeast` comparison the store already
has (`internal/store/users.go:29-35`).

**Site ladder** — `users.site_role`, one value per account:

| | Value | Can |
|---|---|---|
| 1 | `user` | nothing site-wide; the default |
| 2 | `sysop` | manage accounts and invitations, run migrations — everything a site admin does today, except acting on a system owner |
| 3 | `owner` | + grant and revoke site roles, including ownership |

**Group ladder** — `memberships.role`, one value per membership:

| | Value | Can |
|---|---|---|
| 1 | `viewer` | read every survey and result in the group |
| 2 | `editor` | + create and edit surveys in the group |
| 3 | `admin` | + manage the group's memberships and invitations |
| 4 | `owner` | + delete the group, transfer ownership, act on its admins |

In prose these are the *group survey viewer*, *group survey editor*, *group
admin* and *group owner*. The stored values stay short because the scope is
already implied by the table they live in.

Note what the group ladder does **not** have: any notion of a role on a single
survey. A group survey viewer reads everything in the group. Per-survey grants
are deliberately deferred — see [What I am not proposing](#what-i-am-not-proposing).

Every one of the 17 `requireRole` call sites in `internal/web/server.go:235-257`
has to say which ladder it means. That is mechanical but it is where mistakes
will hide, so the helper should be split into two obviously different names
rather than one function with a flag.

### Acting on another account

"A sysop cannot change site ownership" falls straight out of the ladder:
`requireSiteRole(owner)` on the grant-and-revoke path, and sysop does not reach
it. That much needs no special case.

What does need one is the other direction — a sysop must not be able to reach
*through* an owner's account. And that exception is broader than it first
looks. Today `POST /admin/users` with action `reset-link`
(`internal/web/admin.go:468-475`) is gated on `RoleAdmin` alone and mints a
reset link for any named account, handing it straight to the caller. A sysop
points that at the system owner, uses the link, signs in as the owner, and the
ownership rule above is worth nothing. Session revocation and role change are
the same shape: each is a way to act on an account rather than a way to be one.

So the carve-out covers every account-affecting operation — delete, ban,
demote, mint a reset link, revoke sessions — not just deletion and banning. It
is still a single rule, and still `AtLeast`, with a variable on the right
instead of a constant:

```go
// MayActOn reports whether an actor holding role a may perform an
// account-affecting operation on an account holding role target.
func (a Role) MayActOn(target Role) bool { return a.AtLeast(target) }
```

Sysop ≥ sysop, so sysops manage each other. Sysop < owner, so the whole class
is blocked at once rather than enumerated one operation at a time. Owner ≥
owner, so ownership stays transferable and a co-owner can be demoted.

The same helper does duty in the group dimension: a group admin may not remove
or demote a group owner, by the identical predicate against membership roles.

**This belongs in the store, not in the handlers.** Five handler-level checks
is five chances to forget, and the reset-link path is the one people forget.

---

## Schema sketch

```sql
CREATE TABLE groups (
  id           TEXT PRIMARY KEY,     -- opaque, random, appears in URLs
  name         TEXT NOT NULL,        -- display only; NOT unique, by design
  personal_for TEXT REFERENCES users(id) ON DELETE RESTRICT,  -- NULL for ordinary groups
  created      TEXT NOT NULL,
  created_by   TEXT NOT NULL REFERENCES users(id) ON DELETE SET NULL,
  delete_at    TEXT                  -- NULL = live; set = marked, see below
);

CREATE UNIQUE INDEX groups_personal ON groups(personal_for)
  WHERE personal_for IS NOT NULL;    -- at most one personal group per account

CREATE TABLE memberships (
  user_id  TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
  group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  role     TEXT NOT NULL,            -- viewer | editor | admin | owner
  created  TEXT NOT NULL,
  PRIMARY KEY (user_id, group_id)
);
CREATE INDEX memberships_by_group ON memberships(group_id);

ALTER TABLE surveys ADD COLUMN group_id TEXT REFERENCES groups(id);  -- backfilled, then required
ALTER TABLE invites ADD COLUMN group_id TEXT REFERENCES groups(id);  -- an invite is to a group
ALTER TABLE users   ADD COLUMN site_role TEXT NOT NULL DEFAULT 'user';
ALTER TABLE users   ADD COLUMN delete_at TEXT;   -- marked accounts, see below
```

`resets` stays site-scoped: a password belongs to an account, not to a group.
Its `user_name` and `created_by` become `user_id` and `created_by_id` under
step 0.

`personal_for` is what makes a personal group undeletable and what lets the
account-deletion path find it. It is not an authorization input — a personal
group authorizes exactly like any other, through `memberships`. Its
`ON DELETE RESTRICT` is deliberate: an account row must never vanish out from
under its own group.

## Survey creation must name a group

Because surveys never move, the group chosen at creation is permanent. The
create form therefore **asks** which group, defaulting to nothing, and must not
silently drop the survey into the creator's personal group.

This is not a UI nicety. The failure it prevents is someone building a survey,
publishing it, collecting responses, and only then discovering their colleagues
can never see any of it and there is no way to hand it over.

---

## Deletion is always deferred

**Decided (Q10, and Q7 adopts the same mechanism).** Nothing about a group is
destroyed on the spot. Deleting a group sets `groups.delete_at` to now plus the
grace period (30 days), and the row is reclaimed after that.

This is an **undo window, not a retrieval window.** Its purpose is to make a
mistaken deletion reversible, not to give anyone a period in which to extract
data they would not otherwise be able to read. Nobody gains any read they did
not already have — in particular the Q3 decision below is untouched, and a
sysop cannot see into a marked group any more than a live one.

**Marked means inert, immediately:**

- The group disappears from every listing and every switcher.
- Its surveys stop accepting responses at once. `/s/{id}` behaves as it does
  for a closed survey.
- Its members keep their memberships but see nothing; only a restore brings it
  back.

**Evaluated on read, swept on a schedule.** A group past its `delete_at` is
inert the instant the clock passes it, decided at read time — the same idiom
the codebase already uses for `ClosedByClock`
(`internal/store/survey.go:106-109`). The nightly job only reclaims storage.
That way a missed or failed sweep is a storage leak, never a data-visibility
bug, which matters because the sweep is the one genuinely destructive piece of
automation in the system.

It does not belong in the existing backup CronJob (`deploy/k8s/backup.yaml`),
whose ServiceAccount is deliberately scoped to prune its own snapshots and
nothing else. It gets its own `quicksurvey expire` subcommand and its own job.

**Deleting an account marks rather than deletes.** The account's `delete_at`
and its personal group's are set together and expire together. The account is
disabled at once — cannot sign in, sessions revoked — but its row survives the
window, which is what keeps the personal group from going ownerless and what
makes the undo a real undo rather than a partial one.

Restoring a marked account and its personal group is a site-ladder operation,
since it is an account action and requires reading nothing inside the group.

---

## Migrating the live data

This is the part most likely to go wrong, and it is not a column addition.
There are real surveys and accounts in production now; every survey needs a
group and every user needs a membership, or they disappear from the interface.

The plan, assuming step 0 (surrogate keys) has already shipped:

1. Create a personal group for every existing account.
2. Create the destination group for the existing surveys — **named as a
   migration parameter, not hard-coded** (see below).
3. Give every existing survey that group's id.
4. Give every existing user a membership in it, carrying their current role,
   mapped `viewer`→`viewer`, `editor`→`editor`, `admin`→`admin`. Nobody is
   made a group owner by the migration except as step 5 dictates.
5. Set exactly one account's `site_role` to `owner`, also a migration
   parameter, and make it the destination group's owner. Promote every other
   existing `admin` account to `sysop`, since they had site-level powers before
   and taking them away silently would lock people out.
6. Make `surveys.group_id` non-null only after the backfill has been verified.

**Q1 is withdrawn.** The migrated group's name was a design decision only
because the migration was assumed to hard-code it. Taking the destination group
and the system owner as parameters costs the same amount of operator attention,
avoids hand-written `UPDATE`s against production afterwards, and keeps the
rehearsal honest — you rehearse the actual final state rather than an
intermediate one you plan to correct by hand. It is doubly moot now that group
names are neither unique nor addressable.

**Rehearsed against a copy of production before it goes near the cluster.** A
schema change that only added a column took the site down last night; one that
backfills relationships gets a dress rehearsal. This applies to step 0 at least
as much as to the steps above.

---

## Where "one at a time" lives

Not specified, and it is load-bearing.

**URL-scoped** — `/g/{id}/admin/...`. The group is in the address, so it is
bookmarkable, shareable, survives two tabs on two groups, and is available to
authorize against *before* any query runs. A switcher is then just navigation.

**Session-scoped** — a "current group" on the session. Fewer URL changes, but
two tabs fight, and an admin link pasted to a colleague means something
different depending on hidden state.

**Q2. URL-scoped or session-scoped?**
*Recommendation:* URL-scoped. The deciding argument is not convenience: with
the group in the path, "did this handler check the group?" is a mechanical
audit rather than a matter of remembering, and this is the one change where a
missed check means one customer reads another's data. Opaque group IDs remove
the objection that URL-scoping puts a customer's name in the address bar — it
puts a random string there instead, which discloses nothing.

Personal groups sharpen this. There is now at least one group per account, so
the switcher is never empty and never has exactly one entry — which makes
"which group am I looking at?" a question the interface has to answer on every
page, not an edge case. With names non-unique, the switcher must also cope with
two groups that read identically; showing the owner alongside the name is
probably enough, but it is a real UI problem rather than a hypothetical one.

### Respondent URLs stay as they are

`/s/{id}` does not become `/g/{gid}/s/{id}`. Survey IDs are already 50 bits of
randomness, and adding the group to the link gives every respondent a
correlator for "these two surveys came from the same place". That is a
disclosure with no compensating benefit.

---

## What a site owner or sysop can see

**Decided (was Q3): nothing, without a membership.**

Reading a survey or its results requires a role on the group that owns it. The
site ladder grants site powers — accounts, invitations, migrations — and
confers no group role. A system owner with no membership in a group sees
exactly what a stranger sees.

This is not merely a policy choice layered on top; it is what the two-ladder
model already says, and writing it any other way would mean a second
authorization path into group data. One path is the whole point.

A sysop or owner can of course mint a reset link and take over an account that
*is* a member — but "can escalate, visibly" and "can read, silently" are
different promises, and this application's whole pitch is discretion.

If a break-glass path is wanted later, it is an explicit action that writes an
audit line naming the group, the actor and the reason. Not ambient visibility,
and not in the first version.

---

## Invitations

An invitation is now to a *group*, and the invitee may already have an account.

| Case | Behaviour |
|---|---|
| No account, opens the link | Creates the account (and its personal group), adds the membership |
| Signed in, no membership in that group | Adds the membership to the signed-in account |
| Signed in, already a member of that group | **Q4** |
| Signed in as someone else | **Q5** |
| Not signed in but the account exists | Must sign in first, then the link adds the membership |

**Decided (was Q6): a group admin's invitation does not need site approval for
the membership.** A group admin inviting someone *is* the decision, and
requiring a sysop to countersign makes groups useless. Creating a brand-new
*account* still does, since an account is a site-level object. So: an existing
user joins immediately; a new user creates an account that waits for site
approval, and joins once approved.

**Q4. An invitation for someone who is already a member of that group.**
*Recommendation:* Treat it as a role change if the roles differ, and a no-op
otherwise — either way say plainly what happened rather than reporting success
ambiguously. Note that with `MayActOn` in force, an invitation that would
*lower* a group owner's role must be refused for the same reason a direct
demotion would be.

**Q5. Someone opens a group invitation while signed in as a different account.**
*Recommendation:* Show whose account it would join and make them confirm, with
a "sign in as someone else" link. Silently adding the membership to whoever
happens to be signed in is how a colleague's laptop ends up with access.

---

## Group lifecycle

Anyone signed in may create a group and becomes its **owner**. That is not an
abuse vector here, because accounts are invitation-only: anyone able to create
a group was already vouched for.

**Decided (was Q7): the group owner may delete a group**, with the name retyped
as confirmation, and — per [Deletion is always deferred](#deletion-is-always-deferred)
— a 30-day mark rather than an immediate cascade. The earlier answer was "site
admin only" because there was no owner role to hand it to; now there is, and
deleting a group is exactly the power that distinguishes owner from admin. A
group admin still cannot, which preserves the original concern: deleting a
group takes other people's work with it, and that is more than "administer this
group" should mean.

A system owner may also delete any group, by the site ladder. A sysop may not,
since a group is not an account-level object.

Retyping a non-unique name as confirmation is weaker than it was when names
were unique — it confirms attention, not identity. The confirmation should show
the group's ID and survey count alongside the name.

**Decided (was Q9): others may be invited into a personal group.** It is an
ordinary group that happens to have been created automatically, and forbidding
it would mean a second kind of group with its own rules everywhere memberships
are touched. The owner cannot be removed or demoted — that follows from
`MayActOn` plus the last-owner guard — so "personal" means *permanently yours*,
not *only yours*.

---

## Guards

- **Last site owner.** Cannot be demoted or deleted. Without this, `MayActOn`
  can be satisfied into a state where nobody can grant site roles again.
- **Last group owner.** Every group has at least one owner; the last one cannot
  be removed from the group or demoted within it. Losing it leaves the group
  undeletable and its memberships unmanageable.
- **Your own personal group.** You are its owner permanently — you cannot be
  removed from it, demoted in it, or delete it. Falls out of the two guards
  above, but worth asserting directly in a test, because it is the invariant
  the rest of the personal-group behaviour assumes.
- **Removing your own membership.** Same rule as deleting your own account:
  refuse, so nobody locks themselves out of an ordinary group with one click.
- **No ownerless window.** Because account deletion marks rather than deletes,
  a personal group never outlives its owner's row. `personal_for` is
  `ON DELETE RESTRICT` so that a bug cannot produce that state quietly.

---

## Where this will break if it breaks

Every store read needs a group filter, and a missed one is a cross-group leak
rather than an error anybody notices. The list:

| Function | Needs |
|---|---|
| `Surveys()` | a group argument; today it returns everything |
| `Survey(id)` | the caller's membership checked before it is returned |
| `Tally`, `Responses`, `Count` | reachable only through an authorized survey |
| exports | same |
| `Invites()` | scoped to one group |
| the tally cache | keyed on survey id, which is fine — but the *authorization* to read it must happen before the cache is consulted, not after |
| every read above | must also exclude groups past `delete_at`, or a marked group stays visible until the sweep runs |
| the account-management paths | `MayActOn` before delete, ban, demote, reset-link and session revocation — the reset-link path especially |

The tests for this are ordering and authorization tests, not coverage: "a user
in group A cannot reach anything in group B" has to be asserted for every
route, because each one is a separate opportunity to forget. The site ladder
needs its own pair: "a sysop cannot act on a system owner" asserted separately
for each of the five account-affecting operations, since a rule enforced in
four places out of five is not enforced.

---

## Audit logging

Every log line today says `by=admin` with no indication of where. Once there
are groups, every survey and account event needs the group on it, or the log
cannot answer "who did what, to whose data".

Log lines record the **ID and the name at the time**, not the ID alone — a log
that can only be read by joining against rows that may since have been deleted
is not an audit log. Account events additionally need the target's site role at
the time, so a refused or permitted `MayActOn` is reconstructable after the
fact.

---

## What I am not proposing

- **Moving surveys between groups.** Confirmed out of scope. The cost is real
  and worth stating plainly rather than burying: a survey created in the wrong
  group is there forever, there is no copy feature either, and the only remedy
  is to rebuild it and lose the responses. Two things in this document exist to
  keep that from biting — the create form asks which group and refuses to
  guess, and account deletion defers rather than stranding surveys. If those
  turn out to be insufficient in practice, the thing to add is a *copy*, which
  leaves the original responses where the respondents left them.
- **Per-survey roles.** The group ladder grants across the whole group: a group
  survey viewer reads every survey in it. Per-survey grants are a third
  relation and a second authorization boundary, and roughly double the work in
  sequencing step 2. Deferred deliberately, and the schema above does not
  foreclose it — a future `survey_grants` table would narrow what a membership
  already allows, never widen it.
- **Username discriminators.** Described under
  [Names, IDs, and enumeration](#the-same-oracle-exists-today-for-usernames),
  deferred, and unblocked rather than solved by the surrogate-key work.
- **Per-group quotas or rate limits.** The current limits are per address and
  bound abuse adequately. Worth revisiting only if a group becomes a noisy
  neighbour.
- **Per-group branding or custom domains.** Say if you want it; it changes the
  ingress story substantially.

---

## Sequencing

0. **Surrogate keys.** `users.id`, the five existing columns converted to real
   foreign keys with explicit `ON DELETE` rules, `users.name` demoted to a
   unique mutable attribute. Ships and deploys on its own, before any group
   work, and is invisible to users. Doing it first is what keeps `memberships`
   from being built on the thing being removed.
1. Schema, backfill migration, and the upgrade rehearsal against production
   data.
2. Model and store changes with the group filter on every read and `MayActOn`
   on every account-affecting write, plus the cross-group and cross-ladder
   authorization tests, before any UI exists.
3. URL scoping and the group switcher.
4. Invitation changes.
5. Deferred deletion: `delete_at`, the read-time checks, the restore path, and
   the `expire` subcommand with its own CronJob.
6. Site surface (accounts, migrations) split out from the group surface.

Each step ships and is deployable on its own; step 2 is where the security
property is either established or lost.

---

## Questions still open

| | Question | Recommendation |
|---|---|---|
| Q2 | URL-scoped or session-scoped | URL-scoped, with opaque group IDs |
| Q4 | Invitation for an existing member | Role change if different, else no-op, said plainly; refuse if it would demote an owner |
| Q5 | Invitation opened while signed in as someone else | Confirm whose account, offer to switch |

## Decided

| | Question | Decision |
|---|---|---|
| — | What relationships key on | Opaque surrogate IDs, never usernames or group names |
| — | Group names | Display-only, not unique; opaque IDs in URLs; one `/g/` prefix for all groups |
| Q1 | Name of the migrated group | Withdrawn — a migration parameter, not a design decision |
| Q3 | May a site owner or sysop read a group's results | No, and structurally so: site roles confer no group role |
| Q6 | Does a group invite need site approval | Not for the membership; yes for a new account |
| Q7 | Who may delete a group | The group owner, or a system owner; deferred 30 days |
| Q8 | Reserved slugs | Withdrawn — no human-chosen string appears in a path |
| Q9 | May others join a personal group | Yes — ordinary group, permanently owned |
| Q10 | Deletion grace period | 30-day undo window, marked-and-inert immediately, all group deletions alike |
