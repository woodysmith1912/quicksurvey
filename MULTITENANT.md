# Multi-tenancy — design

**Status: draft for review. Nothing here is built.**

Open questions are marked **Q1**…**Qn** and each has a recommendation. Answer
them inline in this file — write under the question, or strike the
recommendation and put your own. I will treat what is in the file as the
decision and update the rest of the document to match before writing code.

---

## What this changes

Today the application has no ownership at all. `Surveys()` returns every survey
in the database, and anyone who can reach `/admin/` sees all of them. There is
one instance-wide role per account and that is the whole authorization model.

Multi-tenancy introduces the first authorization boundary the application has
ever had. That is a bigger change than adding a column: it means every read has
to answer "on whose behalf?", and the failure mode of getting it wrong is one
customer reading another's results.

## The model

```
tenant       a collection of users and surveys
survey       belongs to exactly one tenant, permanently
user         one account, site-wide
membership   (user, tenant, role) — a user may have many
```

A user works within one tenant at a time. A survey never moves between tenants;
if that is ever wanted it is a copy, not a move, because responses are anonymous
and moving them would change who can see them.

### Roles become two-dimensional

The single `users.role` column cannot express this any more, because "admin"
now means two different things.

| | Scope | Can |
|---|---|---|
| **Site role** | the instance | reset passwords, delete accounts, manage tenants, run migrations |
| **Tenant role** | one membership | `viewer` / `editor` / `admin`, exactly as today but bounded to one tenant |

`users.role` becomes the *site* role, with values `user` and `admin`. Everything
that is a tenant capability moves into `memberships.role`.

Every one of the 15 `requireRole` call sites has to say which dimension it
means. That is mechanical but it is where mistakes will hide, so the helper
should be split into two obviously different names rather than one function with
a flag.

### Schema sketch

```sql
CREATE TABLE tenants (
  id       TEXT PRIMARY KEY,
  slug     TEXT NOT NULL UNIQUE,   -- appears in URLs
  name     TEXT NOT NULL,
  created  TEXT NOT NULL,
  created_by TEXT NOT NULL
);

CREATE TABLE memberships (
  user_name TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  role      TEXT NOT NULL,          -- viewer | editor | admin
  created   TEXT NOT NULL,
  PRIMARY KEY (user_name, tenant_id)
);

ALTER TABLE surveys  ADD COLUMN tenant_id TEXT;  -- backfilled, then required
ALTER TABLE invites  ADD COLUMN tenant_id TEXT;  -- an invite is to a tenant
ALTER TABLE users    ADD COLUMN site_role TEXT NOT NULL DEFAULT 'user';
```

`resets` stays site-scoped: a password belongs to an account, not to a tenant.

---

## Migrating the live data

This is the part most likely to go wrong, and it is not a column addition. There
are real surveys and accounts in production now; every survey needs a tenant and
every user needs a membership, or they disappear from the interface.

The plan:

1. Create one tenant, from the instance's existing content.
2. Give every existing survey that tenant's id.
3. Give every existing user a membership in it, carrying their current role.
4. Promote existing `admin` accounts to site admin as well, since they had
   site-level powers before and taking them away silently would lock people out.
5. Make `surveys.tenant_id` non-null only after the backfill has been verified.

**Rehearsed against a copy of production before it goes near the cluster.** A
schema change that only added a column took the site down last night; one that
backfills relationships gets a dress rehearsal.

**Q1. What should the migrated tenant be called?**
*Recommendation:* name `Aligned Software`, slug `aligned`. It is the only
existing content and naming it after the instance owner is honest. Alternative
is a neutral `default`, which ages badly the moment there is a second tenant.

---

## Where "one at a time" lives

Not specified, and it is load-bearing.

**URL-scoped** — `/t/{slug}/admin/...`. The tenant is in the address, so it is
bookmarkable, shareable, survives two tabs on two tenants, and is available to
authorize against *before* any query runs. A switcher is then just navigation.

**Session-scoped** — a "current tenant" on the session. Fewer URL changes, but
two tabs fight, and an admin link pasted to a colleague means something
different depending on hidden state.

**Q2. URL-scoped or session-scoped?**
*Recommendation:* URL-scoped. The deciding argument is not convenience: with the
tenant in the path, "did this handler check the tenant?" is a mechanical audit
rather than a matter of remembering, and this is the one change where a missed
check means one customer reads another's data.

### Respondent URLs stay as they are

`/s/{id}` does not become `/t/{slug}/s/{id}`. Survey IDs are already 50 bits of
randomness, and putting the tenant in the link tells every respondent who is
running the survey. That is a disclosure with no compensating benefit.

---

## What a site admin can see

A site admin can reset any password, so they can always *obtain* access to a
tenant. But "can escalate, visibly" and "can read, silently" are different
promises, and this application's whole pitch is discretion.

**Q3. Can a site admin read a tenant's surveys and results without being a
member?**
*Recommendation:* No. If a break-glass path is wanted, make it an explicit
action that writes an audit line naming the tenant and the reason, rather than
ambient visibility.

---

## Invitations

An invitation is now to a *tenant*, and the invitee may already have an account.

| Case | Behaviour |
|---|---|
| No account, opens the link | Creates the account, adds the membership |
| Signed in, no membership in that tenant | Adds the membership to the signed-in account |
| Signed in, already a member of that tenant | **Q4** |
| Signed in as someone else | **Q5** |
| Not signed in but the account exists | Must sign in first, then the link adds the membership |

**Q4. An invitation for someone who is already a member of that tenant.**
*Recommendation:* Treat it as a role change if the roles differ, and a no-op
otherwise — either way say plainly what happened rather than reporting success
ambiguously.

**Q5. Someone opens a tenant invitation while signed in as a different
account.**
*Recommendation:* Show whose account it would join and make them confirm, with a
"sign in as someone else" link. Silently adding the membership to whoever
happens to be signed in is how a colleague's laptop ends up with access.

**Q6. Does a tenant admin's invitation still need site approval?**
*Recommendation:* No for the membership — a tenant admin inviting someone *is*
the decision, and requiring a site admin to countersign makes tenants useless.
Yes, still, for creating a brand-new *account*, since that is a site-level
object. So: existing user joins immediately; new user creates an account that
waits for site approval, then joins.

---

## Tenant lifecycle

Anyone signed in may create a tenant and becomes its admin. That is not an abuse
vector here, because accounts are invitation-only: anyone able to create a
tenant was already vouched for.

**Q7. Who may delete a tenant, and what happens to its surveys?**
*Recommendation:* Site admin only, with the name retyped as confirmation, and a
cascade to surveys, responses and memberships. A tenant admin deleting a tenant
takes other people's work with it, which is more than "administer this tenant"
should mean.

**Q8. Reserved slugs.**
*Recommendation:* If URL-scoped, refuse `s`, `admin`, `login`, `logout`,
`invite`, `reset`, `account`, `static`, `healthz`, `t`, and anything not
matching `^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`.

---

## Guards that become per-tenant

- **Last admin.** Two separate guards now: the last *site* admin cannot be
  demoted or deleted, and the last *admin of a tenant* cannot be removed from
  it. Losing either leaves something unadministrable.
- **Removing your own membership.** Same rule as deleting your own account:
  refuse, so nobody locks themselves out with one click.

---

## Where this will break if it breaks

Every store read needs a tenant filter, and a missed one is a cross-tenant leak
rather than an error anybody notices. The list:

| Function | Needs |
|---|---|
| `Surveys()` | a tenant argument; today it returns everything |
| `Survey(id)` | the caller's membership checked before it is returned |
| `Tally`, `Responses`, `Count` | reachable only through an authorized survey |
| exports | same |
| `Invites()` | scoped to one tenant |
| the tally cache | keyed on survey id, which is fine — but the *authorization* to read it must happen before the cache is consulted, not after |

The tests for this are ordering and authorization tests, not coverage: "user in
tenant A cannot reach anything in tenant B" has to be asserted for every route,
because each one is a separate opportunity to forget.

---

## Audit logging

Every log line today says `by=admin` with no indication of where. Once there are
tenants, every survey and account event needs the tenant on it, or the log
cannot answer "who did what, to whose data".

---

## What I am not proposing

- **Moving surveys between tenants.** Responses are anonymous and their
  visibility is defined by the tenant; moving them changes who can read answers
  people gave under a different expectation.
- **Per-tenant quotas or rate limits.** The current limits are per address and
  bound abuse adequately. Worth revisiting only if a tenant becomes a noisy
  neighbour.
- **Per-tenant branding or custom domains.** Say if you want it; it changes the
  ingress story substantially.

---

## Sequencing

1. Schema, backfill migration, and the upgrade rehearsal against production data.
2. Model and store changes with the tenant filter on every read, plus the
   cross-tenant authorization tests, before any UI exists.
3. URL scoping and the tenant switcher.
4. Invitation changes.
5. Site-admin surface split out from the tenant-admin surface.

Each step ships and is deployable on its own; step 2 is where the security
property is either established or lost.

---

## Open questions, collected

| | Question | Recommendation |
|---|---|---|
| Q1 | Name of the migrated tenant | `Aligned Software` / `aligned` |
| Q2 | URL-scoped or session-scoped | URL-scoped |
| Q3 | May a site admin read a tenant's results | No, break-glass only, audited |
| Q4 | Invitation for an existing member | Role change if different, else no-op, said plainly |
| Q5 | Invitation opened while signed in as someone else | Confirm whose account, offer to switch |
| Q6 | Does a tenant invite need site approval | Not for the membership; yes for a new account |
| Q7 | Who may delete a tenant | Site admin, confirmed, cascading |
| Q8 | Reserved slugs and slug format | As listed |
