# Shown-to counts — design

**Status:** approved 2026-09-11. Implementation plan follows.

## Problem

The tally reports votes per option and each option's share of all
respondents. That share is wrong for any option that was not on every ballot:
a write-in approved halfway through, or an option an editor added late, has
been seen by fewer people, so its raw count and its share both under-read.

The fix is to record, per respondent, which options were on the ballot they
submitted, and report for each option how many respondents were shown it and
what fraction of those picked it.

## Decisions

| Question | Decision |
|---|---|
| What is "shown"? | Options on the ballot at the moment a respondent **submitted**. People who loaded the page and never submitted are not counted. |
| Where recorded? | At submit time, in the same transaction as the choices. No write on page view. |
| Resubmission | The seen set is the **union** across all of a respondent's submissions. Once shown, always shown. |
| Merges | Resolve seen options through merge pointers exactly as votes are. A respondent who saw both X and Y where X merged into Y counts once for Y. |
| Presentation | **Keep** Share (of all respondents). **Add** Shown to (count) and Interest (% of those shown). |
| Existing data | One-time backfill: every existing response is treated as having seen every approved, removed, or merged option in its survey at migration time. A pending or rejected option is backfilled as seen only by the response that voted for it, since that response can only be its proposer — nobody else could have had it on their ballot. |
| Wide export | `1` picked, `0` shown and not picked, **blank** never shown. |

The "Existing data" row was not the original decision. The original choice was
the blunter rule above applied to every option regardless of status. A live
upgrade test against a real 0.3.0 container showed what that cost: a write-in
left pending across the upgrade and later approved reported shown 5 and
interest 20% for a survey where the only person who had ever seen it was the
one respondent who picked it — a true interest of 100%. The rule was narrowed
to the one above as a result.

## Data model

New table, same shape and cascade as `choices`:

```sql
CREATE TABLE IF NOT EXISTS seen (
  response_id TEXT NOT NULL REFERENCES responses(id) ON DELETE CASCADE,
  option_id   TEXT NOT NULL,
  PRIMARY KEY (response_id, option_id)
);
```

`Response` gains `Seen []string` and `Saw(id string) bool`. `loadResponse` and
`Responses()` load `seen` rows alongside `choices`.

### Backfill

Run once, inside `migrate()`, guarded by a `meta` key (`seen_backfilled`):

```sql
INSERT OR IGNORE INTO seen (response_id, option_id)
SELECT r.id, o.id FROM responses r JOIN options o ON o.survey_id = r.survey_id;
```

Idempotent by construction (`OR IGNORE`), and the key stops it re-running so
options added after the upgrade are not retroactively marked seen by old
responses. `seen` is a new table, not a new column, so it is not an
`addedColumns` entry; the schema text alone creates it.

## Submit path

`saveResponseTx` already knows what was visible to this respondent, because
the carry-forward logic needs the same rule: an option is visible if it is
approved, or if it is pending and the respondent proposed it in this request
(`allowPending`) or had previously chosen it. That rule is extracted into one
helper and used for both filtering choices and computing the seen set.

```
visible  = { o : approved } ∪ { o : pending ∧ (o == allowPending ∨ prev.Chose(o)) }
r.Seen   = visible ∪ prev.Seen
```

Written by delete-and-insert on `seen`, as `choices` is.

Write-ins call `saveResponseTx` with `allowPending` set to the new option, so
the proposer is recorded as having seen their own suggestion from the moment
it exists. Preview responses go through the same path and are cleared on first
publish through the existing cascade.

**Accepted race:** an option approved between page load and submit is recorded
as shown-not-picked. `choices` already has the identical window.

## Tally

`Result` gains:

```go
Shown        int     // distinct respondents whose seen set resolves to this option
ShownPercent float64 // 100 * Votes / Shown; 0 when Shown == 0
```

`computeTally` walks `r.Seen` alongside `r.Choices`, resolving each ID through
`sv.Resolve` and counting each target once per respondent. Sort order
(by votes) is unchanged. The cache stores `[]Result`, so it needs no change;
any submit bumps the generation as before.

Invariant: for every option, `Votes <= Shown <= respondents`. A respondent can
only choose what was on their ballot, and every seen row belongs to a
respondent. The backfill preserves this because it inserts a row for every
(response, option) pair.

## Presentation

`_tally.html` (serves ballot, public results, and admin survey page):

| Option | Votes | Share | Shown to | Interest | bar |
|---|---|---|---|---|---|

Bar is unchanged (votes over respondents). Hint text gains one sentence:
"Shown to" is lower than the respondent count for options added after some
people had already answered; "Interest" is the share of those who saw it.

`summary.tsv` columns: `option, votes, respondents, percent_of_respondents,
shown_to, percent_of_shown`.

`responses.tsv`: per-option cell is `1`, `0`, or blank as above. `selections`
is unchanged. In Sheets, `AVERAGE` over a column ignores blanks and yields
percent of shown; `COUNT` yields shown-to.

Dashboard shows response counts only and is unchanged.

## Testing

Store (`internal/store`):

- First submit records the approved options as seen; a removed option is not.
- Resubmit after a write-in is approved adds it; earlier seen rows survive.
- Own pending write-in is seen by its proposer and by nobody else until approved.
- Merge: seen X and seen Y both resolve to Y; a respondent who saw both counts once.
- Remove then restore: `Shown` for that option is the same before and after.
- Tally invariant `Votes <= Shown <= respondents` holds across the ordering tests.
- Tally cache: a new submission changes `Shown`; covered by the existing every-write-path test once `Shown` is asserted.

Migration (`migrate_test.go`):

- A database built from the previous schema with responses and options comes up with one seen row per (response, option), and the `meta` key set.
- Reopening does not add seen rows for an option created after the first open.
- The every-queried-column test picks up the new queries.

Export (`internal/export`):

- Wide: blank for never-shown, `0` for shown-not-picked, `1` for picked.
- Summary: two new columns, `percent_of_shown` is 0.0 when shown is 0.

Web (`internal/web`):

- Tally renders the two new columns with correct values after a second respondent votes on a newly approved write-in.

Playwright (`e2e/tests/writein.spec.ts`):

- After approval, the admin tally row for the write-in shows Shown to = 1 (the proposer) while respondents = 2.

## Out of scope

- Counting page views without a submission.
- Per-option approval or removal timestamps.
- Changing the bar to scale by shown-to.
