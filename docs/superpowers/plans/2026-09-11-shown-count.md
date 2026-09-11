# Shown-to Counts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record which options were on each respondent's ballot when they submitted, and report per option how many respondents were shown it and what share of those picked it, everywhere vote counts appear.

**Architecture:** A new `seen` child table beside `choices`, written in the same transaction as a submission and unioned across a respondent's submissions. `Tally` counts distinct respondents per resolved option from it, `Result` grows two fields, and the one shared tally template plus both TSV exports render them. A one-shot migration backfills existing responses as having seen every option in their survey.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`), `html/template`, Playwright 1.49.1 for browser tests.

**Spec:** `docs/superpowers/specs/2026-09-11-shown-count-design.md`

## Global Constraints

- No new dependencies. `go.mod` is unchanged.
- Run `gofmt -l -w` on every `.go` file you touched before `git add`. A pre-commit hook blocks commits with unformatted staged Go files.
- Run `go vet ./...` before each commit.
- Every write to the database goes through `Store.tx`. Never call `s.db.Exec` for a write outside it.
- Option visibility has exactly one definition: `visibleTo` (Task 1). Do not re-derive it elsewhere.
- Invariant the tests must keep true for every approved option: `Votes <= Shown <= respondents`.
- Every commit message ends with these two lines:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014FfCo2jWCL8pFHVWJ95Syx
  ```
- Test naming follows the repo: `TestWhatHappensWhen…`, sentence-style, asserting behaviour a reader cares about. Use the existing helpers `newStore`, `mustSurvey`, `reopen`, `newHarness`.

---

## File map

| File | Change |
|---|---|
| `internal/store/store.go` | `seen` table in `schema`; `backfillSeen` called from `migrate` |
| `internal/store/response.go` | `Response.Seen`, `Saw`, `visibleTo`, record seen in `saveResponseTx`, load it in `loadResponse` and `Responses`; `Result.Shown`, `Result.ShownPercent`, `computeTally` |
| `internal/store/seen_test.go` | New. Recording, union, merge, restore, interest, invariant |
| `internal/store/migrate_test.go` | Backfill test; `seen` in the every-query check |
| `internal/store/tally_cache_test.go` | Assert `Shown` is invalidated too |
| `internal/export/tsv.go` | Blank cell for never-shown; two summary columns |
| `internal/export/tsv_test.go` | Tests for both |
| `internal/web/templates/_tally.html` | Two columns and a hint sentence |
| `internal/web/web_test.go` | Rendered tally shows shown-to and interest |
| `e2e/helpers.ts`, `e2e/tests/writein.spec.ts` | `shownTo` helper and assertions |
| `README.md`, `DESIGN.md`, `CLAUDE.md` | Document the columns, the model, the invariant |

---

### Task 1: Record and load what each respondent was shown

**Files:**
- Modify: `internal/store/store.go` (schema constant, after the `choices` table, around line 237)
- Modify: `internal/store/response.go` (`Response`, `Clone`, `saveResponseTx`, `loadResponse`, `Responses`)
- Create: `internal/store/seen_test.go`

**Interfaces:**
- Produces: `Response.Seen []string`, `func (r *Response) Saw(id string) bool`, `func visibleTo(o Option, prev *Response, allowPending string) bool`, `func loadIDs(q queryer, query string, args ...any) ([]string, error)`. Task 3 reads `r.Seen`; Task 4 calls `r.Saw`.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/seen_test.go`:

```go
package store

import "testing"

// seenByText returns the option texts a respondent has been shown, which is
// what a reader of these tests wants to assert on.
func seenByText(t *testing.T, s *Store, surveyID, voter string) map[string]bool {
	t.Helper()
	sv, ok := s.Survey(surveyID)
	if !ok {
		t.Fatal("survey vanished")
	}
	r, ok := s.ResponseFor(surveyID, voter)
	if !ok {
		t.Fatalf("no response from %s", voter)
	}
	out := map[string]bool{}
	for _, id := range r.Seen {
		o, ok := sv.Option(id)
		if !ok {
			t.Fatalf("seen references unknown option %s", id)
		}
		out[o.Text] = true
	}
	return out
}

func TestSubmittingRecordsTheApprovedOptionsAsSeen(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo", "Charlie")
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, sv.Options[2].ID, OptRemoved, "")
	}); err != nil {
		t.Fatal(err)
	}
	v := s.VoterID(sv.ID, "a")
	if _, err := s.SaveResponse(sv.ID, v, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	got := seenByText(t, s, sv.ID, v)
	if !got["Alpha"] || !got["Bravo"] || got["Charlie"] {
		t.Errorf("seen = %v, want Alpha and Bravo but not the removed Charlie", got)
	}
	r, _ := s.ResponseFor(sv.ID, v)
	if !r.Saw(sv.Options[1].ID) || r.Saw(sv.Options[2].ID) {
		t.Error("Saw disagrees with Seen")
	}

	// Like everything else, it survives a restart.
	s2 := reopen(t, s)
	defer s2.Close()
	if r, _ := s2.ResponseFor(sv.ID, v); len(r.Seen) != 2 {
		t.Errorf("after reopening, seen = %v, want 2 options", r.Seen)
	}
}

func TestSeenGrowsAcrossSubmissionsAndNeverShrinks(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	alice := s.VoterID(sv.ID, "alice")
	bob := s.VoterID(sv.ID, "bob")
	if _, err := s.SaveResponse(sv.ID, alice, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}

	// Bob suggests Charlie. He has seen it; nobody else has.
	charlie, err := s.AddWriteIn(sv.ID, bob, "Charlie")
	if err != nil {
		t.Fatal(err)
	}
	if got := seenByText(t, s, sv.ID, bob); !got["Charlie"] {
		t.Error("the proposer of a write-in has been shown it")
	}
	if got := seenByText(t, s, sv.ID, alice); got["Charlie"] {
		t.Error("a pending write-in was recorded as shown to someone else")
	}

	// Approval alone changes nothing for Alice; coming back does.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, charlie, OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}
	if got := seenByText(t, s, sv.ID, alice); got["Charlie"] {
		t.Error("approval marked an option as shown to someone who has not been back")
	}
	if _, err := s.SaveResponse(sv.ID, alice, []string{sv.Options[1].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if got := seenByText(t, s, sv.ID, alice); !got["Charlie"] || !got["Alpha"] || !got["Bravo"] {
		t.Errorf("after resubmitting with Charlie on the ballot: %v", got)
	}

	// Bravo is removed and Alice resubmits. Once shown, always shown.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, sv.Options[1].ID, OptRemoved, "")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, alice, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if got := seenByText(t, s, sv.ID, alice); !got["Bravo"] {
		t.Error("a removed option left the seen set on resubmission")
	}

	// A choice is always a subset of what was seen.
	for _, r := range s.Responses(sv.ID) {
		for _, id := range r.Choices {
			if !r.Saw(id) {
				t.Errorf("response %s chose %s without having seen it", r.ID, id)
			}
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store -run 'TestSubmittingRecordsTheApprovedOptionsAsSeen|TestSeenGrowsAcrossSubmissionsAndNeverShrinks' -v`
Expected: compile error, `r.Seen undefined` and `r.Saw undefined`.

- [ ] **Step 3: Add the table to the schema**

In `internal/store/store.go`, directly after the `choices` table inside the `schema` constant (before the closing backtick), add:

```sql
-- Which options were on the ballot a respondent submitted, unioned across all
-- of their submissions. Votes divided by this is the share of people who were
-- shown an option and picked it, which is the only fair figure for an option
-- added after some people had already answered.
CREATE TABLE IF NOT EXISTS seen (
  response_id TEXT NOT NULL REFERENCES responses(id) ON DELETE CASCADE,
  option_id   TEXT NOT NULL,
  PRIMARY KEY (response_id, option_id)
);
```

- [ ] **Step 4: Extend `Response`**

In `internal/store/response.go`, change the struct and `Clone`, and add `Saw`:

```go
type Response struct {
	ID      string    `json:"id"`
	Voter   string    `json:"voter"`
	Choices []string  `json:"choices"` // option IDs
	// Seen is every option that has been on this respondent's ballot at any
	// submission. It only grows: an option removed after they answered stays,
	// so restoring it restores its denominator too.
	Seen    []string  `json:"seen"`
	Comment string    `json:"comment,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Clone returns a deep copy.
func (r *Response) Clone() *Response {
	c := *r
	c.Choices = append([]string(nil), r.Choices...)
	c.Seen = append([]string(nil), r.Seen...)
	return &c
}

// Saw reports whether the given option has ever been on this respondent's ballot.
func (r *Response) Saw(id string) bool {
	for _, s := range r.Seen {
		if s == id {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Name the visibility rule once**

Still in `response.go`, add above `saveResponseTx`:

```go
// visibleTo reports whether an option was on the ballot of a respondent whose
// previous response is prev and who is proposing allowPending in this request:
// every approved option, plus pending write-ins that are their own.
//
// This is the single definition. Choice filtering, carry-forward and the seen
// set all use it, so they cannot drift apart.
func visibleTo(o Option, prev *Response, allowPending string) bool {
	switch o.Status {
	case OptApproved:
		return true
	case OptPending:
		return o.ID == allowPending || (prev != nil && prev.Chose(o.ID))
	}
	return false
}
```

- [ ] **Step 6: Use it in `saveResponseTx` and record the seen set**

Replace the body of `saveResponseTx` from `seen := map[string]bool{}` through the `choices` insert loop with:

```go
	picked := map[string]bool{}
	for _, id := range choices {
		o, ok := sv.Option(id)
		if !ok || picked[id] || !visibleTo(o, prev, allowPending) {
			// Unknown, duplicated, or not on their ballot (someone else's
			// pending write-in, a removed option). Dropped rather than
			// rejected, so a stale form does not lose the whole ballot.
			continue
		}
		picked[id] = true
		r.Choices = append(r.Choices, id)
	}
	// Carry forward selections the respondent could not see.
	//
	// A submission says what someone chose from the ballot in front of them,
	// and that ballot holds approved options plus their own pending write-ins.
	// Anything else they had selected — an option an editor has since removed,
	// or one a moderator merged into another — is absent from the form, so
	// taking the submission literally deletes it.
	//
	// That was silent and lossy. Removing an option and restoring it preserves
	// its votes, but only from people who happened not to resubmit in between;
	// and a merge transfers votes through Resolve, which one later resubmission
	// would undo. Neither leaves a trace in the count.
	if prev != nil {
		for _, id := range prev.Choices {
			if picked[id] {
				continue
			}
			if o, ok := sv.Option(id); ok && !visibleTo(o, prev, allowPending) {
				picked[id] = true
				r.Choices = append(r.Choices, id)
			}
		}
	}

	// Record what was on the ballot. Once shown, always shown: a later
	// submission adds to the set and never removes from it, so an option that
	// is removed and restored keeps the denominator it had.
	shown := map[string]bool{}
	for _, o := range sv.Options {
		if visibleTo(o, prev, allowPending) {
			shown[o.ID] = true
		}
	}
	if prev != nil {
		for _, id := range prev.Seen {
			shown[id] = true
		}
	}
	// Walk the survey's order so the stored list is deterministic.
	r.Seen = make([]string, 0, len(shown))
	for _, o := range sv.Options {
		if shown[o.ID] {
			r.Seen = append(r.Seen, o.ID)
		}
	}

	if sv.AllowComment {
		if comment = strings.TrimSpace(comment); len(comment) > MaxComment {
			comment = comment[:MaxComment]
		}
		r.Comment = comment
	}

	if _, err := tx.Exec(
		`INSERT INTO responses (id, survey_id, voter, comment, created, updated)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(survey_id, voter) DO UPDATE SET
		   comment = excluded.comment, updated = excluded.updated`,
		r.ID, surveyID, voter, r.Comment, dbTime(r.Created), dbTime(r.Updated)); err != nil {
		return nil, err
	}
	for table, ids := range map[string][]string{"choices": r.Choices, "seen": r.Seen} {
		// table is one of two compile-time constants, never caller input.
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE response_id = ?`, r.ID); err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, err := tx.Exec(
				`INSERT INTO `+table+` (response_id, option_id) VALUES (?, ?)`, r.ID, id); err != nil {
				return nil, err
			}
		}
	}
	return r.Clone(), nil
```

Also update the doc comment on `saveResponseTx` so its last sentence reads: "Anything else is dropped rather than rejected, so a stale form does not lose the whole ballot. The options that were visible are recorded as seen, unioned with whatever earlier submissions saw."

- [ ] **Step 7: Load it in `loadResponse` and `Responses`**

Replace `loadResponse` with:

```go
// loadIDs reads a one-column list, draining and closing the rows before it
// returns, so a caller inside a transaction can issue its next query.
func loadIDs(q queryer, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func loadResponse(q queryer, surveyID, voter string) (*Response, error) {
	var r Response
	var created, updated string
	err := q.QueryRow(
		`SELECT id, voter, comment, created, updated FROM responses WHERE survey_id = ? AND voter = ?`,
		surveyID, voter).Scan(&r.ID, &r.Voter, &r.Comment, &created, &updated)
	if err != nil {
		return nil, err
	}
	r.Created, r.Updated = goTime(created), goTime(updated)
	if r.Choices, err = loadIDs(q, `SELECT option_id FROM choices WHERE response_id = ?`, r.ID); err != nil {
		return nil, err
	}
	if r.Seen, err = loadIDs(q, `SELECT option_id FROM seen WHERE response_id = ?`, r.ID); err != nil {
		return nil, err
	}
	return &r, nil
}
```

In `Responses`, replace everything from `crows, err := s.db.Query(` to the end of the function with:

```go
	s.attach(surveyID, "choices", byID, func(r *Response, id string) { r.Choices = append(r.Choices, id) })
	s.attach(surveyID, "seen", byID, func(r *Response, id string) { r.Seen = append(r.Seen, id) })
	return out
}

// attach appends each (response, option) pair in a child table to its response.
// table is one of two compile-time constants, never caller input.
func (s *Store) attach(surveyID, table string, byID map[string]*Response, add func(*Response, string)) {
	rows, err := s.db.Query(
		`SELECT c.response_id, c.option_id FROM `+table+` c
		 JOIN responses r ON r.id = c.response_id WHERE r.survey_id = ?`, surveyID)
	if err != nil {
		slog.Error("could not read "+table, "survey", surveyID, "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var rid, oid string
		if err := rows.Scan(&rid, &oid); err != nil {
			slog.Error("could not read "+table, "survey", surveyID, "err", err)
			return
		}
		if r, ok := byID[rid]; ok {
			add(r, oid)
		}
	}
}
```

- [ ] **Step 8: Run the whole store package**

Run: `go test ./internal/store`
Expected: PASS, including the two new tests and every existing ordering and option-edit test.

- [ ] **Step 9: Format, vet, commit**

```bash
gofmt -l -w internal/store/*.go
go vet ./...
git add internal/store/store.go internal/store/response.go internal/store/seen_test.go
git commit -m "Record which options each respondent was shown

A seen child table beside choices, written in the same transaction as a
submission and unioned across a respondent's submissions. Visibility now
has one definition, visibleTo, shared by choice filtering, carry-forward
and the seen set.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014FfCo2jWCL8pFHVWJ95Syx"
```

---

### Task 2: Backfill responses recorded before the table existed

**Files:**
- Modify: `internal/store/store.go` (`migrate`, new `backfillSeen`)
- Modify: `internal/store/migrate_test.go`

**Interfaces:**
- Consumes: the `seen` table and `Response.Seen` from Task 1.
- Produces: a `meta` row with key `seen_backfilled`. Nothing else depends on it.

- [ ] **Step 1: Write the failing test**

Append to `internal/store/migrate_test.go`:

```go
// Responses recorded before the seen table existed carry no record of what
// was on their ballot. The upgrade treats them as having seen every option
// in their survey — the assumption the old share-of-respondents figure already
// made — and does so exactly once, so options added later are not
// retroactively marked as shown to people who answered before they existed.
func TestSeenIsBackfilledOnceForResponsesFromBeforeItExisted(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	v := s.VoterID(sv.ID, "a")
	if _, err := s.SaveResponse(sv.ID, v, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Turn it into what the previous release would have left behind.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE seen`, `DELETE FROM meta WHERE key = 'seen_backfilled'`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err = Open(dir)
	if err != nil {
		t.Fatalf("opening a database without the seen table failed: %v", err)
	}
	r, ok := s.ResponseFor(sv.ID, v)
	if !ok {
		t.Fatal("the response is gone")
	}
	if len(r.Seen) != 2 {
		t.Fatalf("seen = %v after upgrade, want both options backfilled", r.Seen)
	}

	// An option added after the upgrade was not shown to anyone who answered
	// before it. A second open must not backfill again.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		_, err := AddOption(d, "Charlie", OptApproved, "editor")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if r, _ := s.ResponseFor(sv.ID, v); len(r.Seen) != 2 {
		t.Errorf("seen = %v after a second open, want the backfill to have run only once", r.Seen)
	}
}
```

Also add the new table to the query map in `TestEveryQueriedColumnExistsAfterMigration`:

```go
		"seen":      `SELECT response_id, option_id FROM seen LIMIT 1`,
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store -run TestSeenIsBackfilledOnceForResponsesFromBeforeItExisted -v`
Expected: FAIL, `seen = [] after upgrade, want both options backfilled`.

- [ ] **Step 3: Implement the backfill**

In `internal/store/store.go`, change `migrate` to:

```go
func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("applying schema: %w", err)
	}
	for _, c := range addedColumns {
		if err := s.ensureColumn(c.table, c.column, c.decl); err != nil {
			return fmt.Errorf("adding %s.%s: %w", c.table, c.column, err)
		}
	}
	if err := s.backfillSeen(); err != nil {
		return fmt.Errorf("backfilling seen: %w", err)
	}
	return nil
}

// backfillSeen gives responses recorded before the seen table existed a seen
// set, once. Nothing recorded what was on those ballots, so the assumption is
// the one the share-of-respondents figure already made: everyone saw
// everything. Guarded by a meta key rather than by the table being empty, so
// options added after the upgrade are never marked as shown to people who
// answered before they existed.
func (s *Store) backfillSeen() error {
	return s.tx(func(tx *sql.Tx) error {
		var done int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM meta WHERE key = 'seen_backfilled'`).Scan(&done); err != nil {
			return err
		}
		if done > 0 {
			return nil
		}
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO seen (response_id, option_id)
			 SELECT r.id, o.id FROM responses r JOIN options o ON o.survey_id = r.survey_id`)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			slog.Info("upgrading the database", "table", "seen", "backfilled_rows", n)
		}
		_, err = tx.Exec(`INSERT INTO meta (key, value) VALUES ('seen_backfilled', ?)`, []byte{1})
		return err
	})
}
```

- [ ] **Step 4: Run the migration tests**

Run: `go test ./internal/store -run 'Migration|Schema|Backfilled' -v`
Expected: PASS for all three.

- [ ] **Step 5: Format, vet, commit**

```bash
gofmt -l -w internal/store/*.go
go vet ./...
git add internal/store/store.go internal/store/migrate_test.go
git commit -m "Backfill seen for responses that predate it, once

Existing responses are treated as having seen every option in their
survey, which is what the old share already assumed. A meta key stops
it running twice, so later options are not marked as shown to people
who answered before they existed.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014FfCo2jWCL8pFHVWJ95Syx"
```

---

### Task 3: Count shown-to and interest in the tally

**Files:**
- Modify: `internal/store/response.go` (`Result`, `computeTally`)
- Modify: `internal/store/seen_test.go`
- Modify: `internal/store/tally_cache_test.go`

**Interfaces:**
- Consumes: `Response.Seen` from Task 1.
- Produces: `Result.Shown int`, `Result.ShownPercent float64`. Tasks 4 and 5 read both.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/seen_test.go`:

```go
// shownByText returns Result.Shown keyed by option text.
func shownByText(t *testing.T, s *Store, surveyID string) map[string]int {
	t.Helper()
	results, _ := s.Tally(surveyID)
	out := map[string]int{}
	for _, r := range results {
		out[r.Option.Text] = r.Shown
	}
	return out
}

// checkTallyInvariant insists that for every option, votes <= shown <=
// respondents. A respondent can only pick what was on their ballot, and every
// seen row belongs to a respondent.
func checkTallyInvariant(t *testing.T, s *Store, surveyID string) {
	t.Helper()
	results, respondents := s.Tally(surveyID)
	for _, r := range results {
		if r.Votes > r.Shown || r.Shown > respondents {
			t.Errorf("%s: votes %d, shown %d, respondents %d — invariant broken",
				r.Option.Text, r.Votes, r.Shown, respondents)
		}
	}
}

func TestInterestIsTheShareOfThoseWhoWereShownAnOption(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha")
	a, b := s.VoterID(sv.ID, "a"), s.VoterID(sv.ID, "b")
	if _, err := s.SaveResponse(sv.ID, a, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, b, nil, ""); err != nil {
		t.Fatal(err)
	}
	// a suggests Charlie, which is approved. b never comes back.
	charlie, err := s.AddWriteIn(sv.ID, a, "Charlie")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, charlie, OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}

	results, respondents := s.Tally(sv.ID)
	if respondents != 2 {
		t.Fatalf("respondents = %d, want 2", respondents)
	}
	byText := map[string]Result{}
	for _, r := range results {
		byText[r.Option.Text] = r
	}
	alpha, ch := byText["Alpha"], byText["Charlie"]
	if alpha.Shown != 2 || alpha.Votes != 1 || alpha.Percent != 50 || alpha.ShownPercent != 50 {
		t.Errorf("Alpha = %+v, want shown 2, votes 1, both shares 50", alpha)
	}
	if ch.Shown != 1 || ch.Votes != 1 || ch.Percent != 50 || ch.ShownPercent != 100 {
		t.Errorf("Charlie = %+v, want shown 1, votes 1, share 50 but interest 100", ch)
	}
	checkTallyInvariant(t, s, sv.ID)
}

func TestShownFollowsMergesWithoutDoubleCounting(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	pizza := sv.Options[0].ID
	both := s.VoterID(sv.ID, "both")
	if _, err := s.SaveResponse(sv.ID, both, []string{pizza}, ""); err != nil {
		t.Fatal(err)
	}
	dupe, err := s.AddWriteIn(sv.ID, both, "pizza!!")
	if err != nil {
		t.Fatal(err)
	}
	only := s.VoterID(sv.ID, "only")
	dupe2, err := s.AddWriteIn(sv.ID, only, "PIZZA")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{dupe, dupe2} {
		if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
			return SetOptionStatus(d, id, OptMerged, pizza)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// "both" saw Pizza and its duplicate; "only" saw Pizza and theirs. Two
	// people, not four exposures.
	if got := shownByText(t, s, sv.ID)["Pizza"]; got != 2 {
		t.Errorf("Pizza shown = %d, want 2 — a person who saw an option and its duplicate counts once", got)
	}
	checkTallyInvariant(t, s, sv.ID)
}

func TestRemovingAndRestoringAnOptionKeepsItsShownCount(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	bravo := sv.Options[1].ID
	for _, who := range []string{"a", "b"} {
		if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, who), []string{bravo}, ""); err != nil {
			t.Fatal(err)
		}
	}
	setStatus := func(status string) {
		t.Helper()
		if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
			return SetOptionStatus(d, bravo, status, "")
		}); err != nil {
			t.Fatal(err)
		}
	}
	setStatus(OptRemoved)
	// One person resubmits while it is off the ballot.
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "a"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	setStatus(OptApproved)
	results, _ := s.Tally(sv.ID)
	for _, r := range results {
		if r.Option.ID == bravo && (r.Shown != 2 || r.Votes != 2) {
			t.Errorf("restored Bravo: shown %d votes %d, want 2 and 2", r.Shown, r.Votes)
		}
	}
	checkTallyInvariant(t, s, sv.ID)
}
```

In `internal/store/tally_cache_test.go`, inside `TestTallyCacheIsInvalidatedByEveryWritePath`, add a second helper below `votes` and two assertions:

```go
	shown := func() map[string]int {
		results, _ := s.Tally(sv.ID)
		out := map[string]int{}
		for _, r := range results {
			out[r.Option.Text] = r.Shown
		}
		return out
	}
```

After the existing step-1 assertion (`after a vote, Alpha = ...`), add:

```go
	if got := shown()["Alpha"]; got != 1 {
		t.Errorf("after a vote, Alpha shown = %d, want 1 — the cache was not invalidated", got)
	}
```

After the step-3 assertion (`after approval, Delta = ...`), add:

```go
	if got := shown()["Delta"]; got != 1 {
		t.Errorf("after approval, Delta shown = %d, want 1 (its proposer)", got)
	}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/store -run 'Interest|ShownFollows|KeepsItsShownCount|TallyCacheIsInvalidated' -v`
Expected: compile error, `r.Shown undefined`.

- [ ] **Step 3: Implement**

In `internal/store/response.go`, replace `Result` and `computeTally`:

```go
// Result is one row of a tally.
type Result struct {
	Option  Option
	Votes   int
	Percent float64 // share of all respondents who selected it
	// Shown is how many respondents had this option on their ballot at some
	// submission. It is below the respondent count for an option added after
	// some people had already answered.
	Shown int
	// ShownPercent is Votes as a share of Shown: of the people who saw it,
	// how many picked it. Zero when nobody has been shown it.
	ShownPercent float64
}

// computeTally does the work, without consulting the cache.
func (s *Store) computeTally(surveyID string) (results []Result, respondents int) {
	sv, ok := s.Survey(surveyID)
	if !ok {
		return nil, 0
	}
	counts, shown := map[string]int{}, map[string]int{}
	for _, r := range s.Responses(surveyID) {
		respondents++
		// Two merged options can resolve to the same target; one person must
		// still only count once for it, both as a vote and as an exposure.
		countOnce(sv, r.Choices, counts)
		countOnce(sv, r.Seen, shown)
	}
	for _, o := range sv.Options {
		if o.Status != OptApproved {
			continue
		}
		res := Result{Option: o, Votes: counts[o.ID], Shown: shown[o.ID]}
		if respondents > 0 {
			res.Percent = 100 * float64(res.Votes) / float64(respondents)
		}
		if res.Shown > 0 {
			res.ShownPercent = 100 * float64(res.Votes) / float64(res.Shown)
		}
		results = append(results, res)
	}
	sortResultsByVotes(results)
	return results, respondents
}

// countOnce increments into[target] once per distinct resolved target among ids.
func countOnce(sv *Survey, ids []string, into map[string]int) {
	done := map[string]bool{}
	for _, id := range ids {
		if target, ok := sv.Resolve(id); ok && !done[target] {
			done[target] = true
			into[target]++
		}
	}
}
```

Also update the `Tally` doc comment's first sentence to: "Tally counts votes per approved option, following merges, and how many respondents were shown each one."

- [ ] **Step 4: Run the store package**

Run: `go test -race ./internal/store`
Expected: PASS.

- [ ] **Step 5: Format, vet, commit**

```bash
gofmt -l -w internal/store/*.go
go vet ./...
git add internal/store/response.go internal/store/seen_test.go internal/store/tally_cache_test.go
git commit -m "Count how many respondents were shown each option

Result gains Shown and ShownPercent. Exposures resolve through merges
and count once per respondent, the same way votes do, so votes <= shown
<= respondents holds for every option.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014FfCo2jWCL8pFHVWJ95Syx"
```

---

### Task 4: Exports

**Files:**
- Modify: `internal/export/tsv.go` (`Responses`, `Summary`)
- Modify: `internal/export/tsv_test.go`

**Interfaces:**
- Consumes: `Response.Saw` (Task 1), `Result.Shown`, `Result.ShownPercent` (Task 3).

- [ ] **Step 1: Write the failing tests**

In `internal/export/tsv_test.go`, extend `TestSummaryTSV`. Replace its two `if` assertions with:

```go
	if rows[0][0] != "option" || rows[0][3] != "percent_of_respondents" ||
		rows[0][4] != "shown_to" || rows[0][5] != "percent_of_shown" {
		t.Errorf("header = %v", rows[0])
	}
	if rows[1][0] != "Rock climbing" || rows[1][1] != "3" || rows[1][2] != "4" || rows[1][3] != "75.0" ||
		rows[1][4] != "4" || rows[1][5] != "75.0" {
		t.Errorf("top row = %v, want [Rock climbing 3 4 75.0 4 75.0]", rows[1])
	}
```

Add a new test:

```go
// An option someone never had on their ballot is blank, not 0. In Sheets,
// AVERAGE over the column then gives the share of those shown it, and COUNT
// gives how many were.
func TestAnOptionNeverShownToARespondentIsBlankNotZero(t *testing.T) {
	s, sv := fixture(t)
	if _, err := s.SaveResponse(sv.ID, "v1", []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	late, err := s.AddWriteIn(sv.ID, "v2", "Karaoke")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *store.Survey) error {
		return store.SetOptionStatus(d, late, store.OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, "v3", nil, ""); err != nil {
		t.Fatal(err)
	}
	sv, _ = s.Survey(sv.ID)

	var b strings.Builder
	if err := Responses(&b, sv, s.Responses(sv.ID), time.UTC); err != nil {
		t.Fatal(err)
	}
	rows := grid(b.String())
	col := -1
	for i, h := range rows[0] {
		if h == "Karaoke" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("Karaoke column missing from %v", rows[0])
	}
	// v1 answered before it existed; v2 proposed it; v3 saw it and passed.
	if got := []string{rows[1][col], rows[2][col], rows[3][col]}; got[0] != "" || got[1] != "1" || got[2] != "0" {
		t.Errorf("Karaoke column = %q, want [\"\" \"1\" \"0\"]", got)
	}
	for i, r := range rows {
		if len(r) != len(rows[0]) {
			t.Errorf("row %d has %d fields, want %d", i, len(r), len(rows[0]))
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/export -run 'TestSummaryTSV|TestAnOptionNeverShown' -v`
Expected: both FAIL (missing columns; `""` expected but `"0"` found).

- [ ] **Step 3: Implement**

In `internal/export/tsv.go`, replace the inner column loop of `Responses`:

```go
		for _, o := range cols {
			switch {
			case r.Chose(o.ID):
				rec, n = append(rec, "1"), n+1
			case r.Saw(o.ID):
				rec = append(rec, "0")
			default:
				// Never on this person's ballot. Blank rather than 0, so a
				// column's AVERAGE in Sheets is the share of those who were
				// shown it and its COUNT is how many were.
				rec = append(rec, "")
			}
		}
```

Update the `Responses` doc comment's first sentence to: "Responses writes the wide, one-row-per-response table: a column per option holding 1 (picked), 0 (shown, not picked) or blank (never on that person's ballot), plus the comment."

Replace `Summary`:

```go
// Summary writes the tally: one row per option with its vote count, its share
// of all respondents, how many respondents were shown it, and its share of
// those. Percentages do not sum to 100, because a respondent may thumbs-up any
// number of options.
func Summary(w io.Writer, sv *store.Survey, results []store.Result, respondents int) error {
	bw := bufio.NewWriter(w)
	if err := row(bw, "option", "votes", "respondents", "percent_of_respondents",
		"shown_to", "percent_of_shown"); err != nil {
		return err
	}
	for _, res := range results {
		if err := row(bw, textCell(res.Option.Text), fmt.Sprint(res.Votes), fmt.Sprint(respondents),
			fmt.Sprintf("%.1f", res.Percent), fmt.Sprint(res.Shown),
			fmt.Sprintf("%.1f", res.ShownPercent)); err != nil {
			return err
		}
	}
	return bw.Flush()
}
```

- [ ] **Step 4: Run the export package**

Run: `go test ./internal/export`
Expected: PASS.

- [ ] **Step 5: Format, vet, commit**

```bash
gofmt -l -w internal/export/*.go
go vet ./...
git add internal/export/tsv.go internal/export/tsv_test.go
git commit -m "Export shown-to counts, and blank cells for options never shown

summary.tsv gains shown_to and percent_of_shown. In responses.tsv an
option that was never on someone's ballot is blank rather than 0, so a
column's AVERAGE in Sheets is the share of those shown it.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014FfCo2jWCL8pFHVWJ95Syx"
```

---

### Task 5: Show it in the tally table

**Files:**
- Modify: `internal/web/templates/_tally.html`
- Modify: `internal/web/web_test.go`

**Interfaces:**
- Consumes: `Result.Shown`, `Result.ShownPercent` (Task 3); the `pct` template func already registered in `server.go`.
- Produces: `data-testid="shown"` and `data-testid="interest"` cells. Task 6's Playwright helper reads `shown`.

- [ ] **Step 1: Write the failing test**

Append to `internal/web/web_test.go`:

```go
// tallyRow returns the rendered <tr> for one option, or "" if it is absent.
func tallyRow(body, option string) string {
	re := regexp.MustCompile(`(?s)<tr data-testid="tally-row" data-option="` + regexp.QuoteMeta(option) + `">.*?</tr>`)
	return re.FindString(body)
}

func TestTallyShowsHowManyWereShownEachOptionAndTheirInterest(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	path := "/s/" + sv.ID

	alice, bob := h.browser(), h.browser()
	for _, b := range []*browser{alice, bob} {
		token := b.csrf(path)
		b.follow(b.post(path+"/vote", url.Values{"csrf": {token}, "choice": {sv.Options[0].ID}}))
	}
	token := alice.csrf(path)
	alice.follow(alice.post(path+"/writein", url.Values{"csrf": {token}, "text": {"Sushi"}}))

	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	adminPath := "/admin/s/" + sv.ID
	sv, _ = h.st.Survey(sv.ID)
	pending := sv.PendingOptions()
	token = editor.csrf(adminPath)
	editor.follow(editor.post(adminPath+"/moderate", url.Values{
		"csrf": {token}, "option": {pending[0].ID}, "action": {"approve"}, "text": {"Sushi"},
	}))

	body := editor.get(adminPath).body
	pizza, sushi := tallyRow(body, "Pizza"), tallyRow(body, "Sushi")
	if pizza == "" || sushi == "" {
		t.Fatalf("tally rows missing:\n%s", body)
	}
	if !strings.Contains(pizza, `data-testid="shown">2<`) || !strings.Contains(pizza, `data-testid="interest">100%<`) {
		t.Errorf("Pizza row should show shown 2 and interest 100%%:\n%s", pizza)
	}
	// Only Alice has been shown Sushi, so its share of respondents is 50% but
	// its interest is 100%.
	if !strings.Contains(sushi, `data-testid="shown">1<`) || !strings.Contains(sushi, `data-testid="interest">100%<`) {
		t.Errorf("Sushi row should show shown 1 and interest 100%%:\n%s", sushi)
	}
	if !strings.Contains(sushi, ">50%<") {
		t.Errorf("Sushi row should still show its 50%% share of all respondents:\n%s", sushi)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/web -run TestTallyShowsHowManyWereShownEachOptionAndTheirInterest -v`
Expected: FAIL, `Pizza row should show shown 2`.

- [ ] **Step 3: Update the template**

Replace `internal/web/templates/_tally.html` with:

```html
{{define "tally"}}
<p class="hint" data-testid="respondents">{{.Voters}} {{plural .Voters "person has" "people have"}} responded.
  Someone can pick more than one option, so the shares do not add up to 100%.
  An option added after some people had already answered was shown to fewer of them;
  "Interest" is the share of those who saw it.</p>
<table data-testid="tally">
  <thead><tr><th>Option</th><th class="num">Votes</th><th class="num">Share</th><th class="num">Shown to</th><th class="num">Interest</th><th></th></tr></thead>
  <tbody>
  {{range .Results}}
    <tr data-testid="tally-row" data-option="{{.Option.Text}}">
      <td>{{.Option.Text}}</td>
      <td class="num" data-testid="votes">{{.Votes}}</td>
      <td class="num">{{pct .Percent}}</td>
      <td class="num" data-testid="shown">{{.Shown}}</td>
      <td class="num" data-testid="interest">{{pct .ShownPercent}}</td>
      <td><progress class="bar" value="{{.Votes}}" max="{{if $.Voters}}{{$.Voters}}{{else}}1{{end}}"></progress></td>
    </tr>
  {{else}}
    <tr><td colspan="6" class="muted">No options to show yet.</td></tr>
  {{end}}
  </tbody>
</table>
{{end}}
```

- [ ] **Step 4: Run the web package**

Run: `go test ./internal/web`
Expected: PASS. If an existing test asserts on the old hint text or a `colspan="4"`, update that assertion to the new template rather than the template.

- [ ] **Step 5: Format, vet, commit**

```bash
gofmt -l -w internal/web/*.go
go vet ./...
git add internal/web/templates/_tally.html internal/web/web_test.go
git commit -m "Show how many respondents were shown each option

Two columns in the shared tally fragment, so the ballot, the public
results page and the admin page all get them.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014FfCo2jWCL8pFHVWJ95Syx"
```

---

### Task 6: Browser test and documentation

**Files:**
- Modify: `e2e/helpers.ts` (after `tally`)
- Modify: `e2e/tests/writein.spec.ts` (first test)
- Modify: `README.md` ("Import into Google Sheets" section)
- Modify: `DESIGN.md` (new section after "Editing a survey people have already answered")
- Modify: `CLAUDE.md` ("Invariants to preserve")

**Interfaces:**
- Consumes: `data-testid="shown"` from Task 5.

- [ ] **Step 1: Add the helper**

In `e2e/helpers.ts`, after the `tally` function:

```ts
/** Reads the tally's "Shown to" column as option text to respondent count. */
export async function shownTo(page: Page): Promise<Record<string, number>> {
  const out: Record<string, number> = {};
  for (const row of await page.getByTestId('tally-row').all()) {
    out[(await row.getAttribute('data-option'))!] = Number(await row.getByTestId('shown').innerText());
  }
  return out;
}
```

- [ ] **Step 2: Extend the first write-in test**

In `e2e/tests/writein.spec.ts`, change the import to include `shownTo`:

```ts
import { createSurvey, publish, respondent, shownTo, signIn, tally, thumbsUp } from '../helpers';
```

In the test `a suggestion is private until approved, then counts for its author`, right after `await expect(other.getByTestId('option').filter({ hasText: 'Escape room' })).toHaveCount(0);` add:

```ts
    // They answer with nothing selected, so they count as a respondent who
    // was shown Bowling and not the suggestion.
    await other.getByTestId('submit-vote').click();
    await expect(other.getByTestId('flash')).toContainText('recorded');
```

Right after the existing `expect(await tally(page)).toEqual({ Bowling: 1, 'Escape room (downtown)': 1 });` add:

```ts
    // Two people answered, but only the author had the suggestion on their ballot.
    expect(await shownTo(page)).toEqual({ Bowling: 2, 'Escape room (downtown)': 1 });
```

At the end of the test, before `await authorCtx.close();`, add:

```ts
    // Once the other person answers again with it on their ballot, it has been shown to them.
    await other.getByTestId('submit-vote').click();
    await page.goto(s.adminUrl);
    expect(await shownTo(page)).toEqual({ Bowling: 2, 'Escape room (downtown)': 2 });
```

- [ ] **Step 3: Run the spec**

Run: `make build && cd e2e && npx playwright test tests/writein.spec.ts` (needs `make e2e-install` once on this host; otherwise run the whole suite with `make e2e-docker`).
Expected: 4 passed.

- [ ] **Step 4: README**

In `README.md`, under "Import into Google Sheets", replace the two bullets with:

```markdown
- `…-responses-*.tsv` — one row per respondent, a column per option holding
  `1` (picked), `0` (shown and not picked) or blank (never on that person's
  ballot, because it was added after they answered), a selection count, and
  the comment. Pivot this. `AVERAGE` of an option's column is the share of
  people shown it who picked it; `COUNT` is how many were shown it.
- `…-summary-*.tsv` — one row per option: votes, respondents, share of
  respondents, how many were shown it, and share of those.
```

And after the paragraph beginning "Shares are the percentage of *respondents*", add:

```markdown
"Shown to" is how many respondents had an option on their ballot when they
answered. It is lower than the respondent count for an option added later —
an approved write-in, or one an editor added — and "Interest" is votes as a
share of that, which is the fair comparison for such options.
```

- [ ] **Step 5: DESIGN.md**

Insert after the "Editing a survey people have already answered" subsection (before "## Option order"):

```markdown
### Who was shown what

An option approved halfway through a survey has been on fewer ballots than the
rest, so its vote count and its share of respondents both under-read. The
tally therefore also reports, per option, how many respondents were **shown**
it and votes as a share of that.

"Shown" is recorded at submission, not on page view: `seen` is a child table
of `responses` with the same shape as `choices`, written in the same
transaction, holding every option that was visible to that respondent. The
visibility rule — approved options plus their own pending write-ins — has one
definition, `visibleTo`, shared by choice filtering, carry-forward and the
seen set. The set is unioned across a respondent's submissions and never
shrinks, so an option removed and restored keeps its denominator.

Recording on submit rather than on view was deliberate. A write per page load
would invalidate the tally cache on every ballot view, exactly where it earns
its keep; it would count crawlers, link unfurlers and people who looked and
left; and it would store rows for voter identities that never respond. The
denominator this produces is a subset of the existing respondent count, so
the two shares are comparable, and `Votes <= Shown <= respondents` holds for
every option.

Exposures resolve through merges and count once per respondent, the same way
votes do. Responses recorded before the table existed were backfilled once as
having seen every option in their survey — the assumption the old share
already made — under a `meta` key so the backfill cannot run again and mark
later options as shown to people who answered before they existed.
```

- [ ] **Step 6: CLAUDE.md**

In `CLAUDE.md`, under "Invariants to preserve", after the "A resubmission only touches options the respondent could see" bullet, add:

```markdown
- **`seen` records what was on the ballot at submit, unioned across submissions.** Visibility has one definition, `visibleTo` in `response.go`; use it rather than re-deriving approved-plus-own-pending. For every approved option `Votes <= Shown <= respondents`.
```

- [ ] **Step 7: Full verification**

```bash
gofmt -l .            # must print nothing
go vet ./...
go test -race -count=1 ./...
```
Expected: no gofmt output, vet clean, all packages `ok`.

- [ ] **Step 8: Commit**

```bash
git add e2e/helpers.ts e2e/tests/writein.spec.ts README.md DESIGN.md CLAUDE.md
git commit -m "Test shown-to counts in the browser, and document them

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014FfCo2jWCL8pFHVWJ95Syx"
```
