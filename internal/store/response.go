package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Response is one person's current answer to one survey.
//
// It deliberately contains nothing that identifies a human being: no account,
// no IP address, no user agent. Voter is a per-survey pseudonym derived from a
// random browser cookie (see Store.VoterID), which is what makes repeat
// submissions detectable without making respondents identifiable.
//
// That is a claim about this record, not about the deployment. The proxy in
// front logs an IP and a time, and Created/Updated here are timestamps; anyone
// holding both can line them up. See the anonymity section of DESIGN.md.
//
// There is one row per respondent per survey. A second submission replaces the
// first rather than adding to it, which is what stops anyone inflating a count
// by resubmitting. Superseded answers are not retained.
type Response struct {
	ID      string   `json:"id"`
	Voter   string   `json:"voter"`
	Choices []string `json:"choices"` // option IDs
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

// Chose reports whether this response selected the given option.
func (r *Response) Chose(id string) bool {
	for _, c := range r.Choices {
		if c == id {
			return true
		}
	}
	return false
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

// VoterID derives the stored identifier for a respondent of one survey from the
// random token in their cookie.
//
// The derivation is keyed and includes the survey ID, so the same browser
// produces unrelated identifiers on different surveys. Neither the raw token
// nor any identity can be recovered from a stored response.
func (s *Store) VoterID(surveyID, token string) string {
	return s.MAC("voter", surveyID, token)[:22]
}

// MaxComment bounds the free-text field, so one respondent cannot fill the disk.
const MaxComment = 2000

// SaveResponse records a respondent's choices, replacing any answer they
// previously gave.
func (s *Store) SaveResponse(surveyID, voter string, choices []string, comment string) (*Response, error) {
	return s.saveResponse(surveyID, voter, choices, comment, "", false)
}

// PreviewResponse is SaveResponse for an editor trying out a survey that is
// still a draft. Such responses are real records, and are discarded when the
// survey is published for the first time.
func (s *Store) PreviewResponse(surveyID, voter string, choices []string, comment string) (*Response, error) {
	return s.saveResponse(surveyID, voter, choices, comment, "", true)
}

func (s *Store) saveResponse(surveyID, voter string, choices []string, comment, allowPending string, preview bool) (*Response, error) {
	var out *Response
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		out, err = saveResponseTx(tx, surveyID, voter, choices, comment, allowPending, preview)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

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

// saveResponseTx is the body of SaveResponse, inside a caller's transaction.
//
// Choices are filtered against the survey: a respondent may select approved
// options, pending write-ins they already had selected, and allowPending, which
// is the option they are proposing in this same request. Anything else is
// dropped rather than rejected, so a stale form does not lose the whole ballot.
// The options that were visible are recorded as seen, unioned with whatever
// earlier submissions saw.
func saveResponseTx(tx *sql.Tx, surveyID, voter string, choices []string, comment, allowPending string, preview bool) (*Response, error) {
	sv, err := loadSurvey(tx, surveyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("survey %q: %w", surveyID, ErrNotFound)
	} else if err != nil {
		return nil, err
	}
	if !sv.AcceptingFrom(time.Now(), preview) {
		return nil, fmt.Errorf("survey is not accepting responses")
	}

	prev, err := loadResponse(tx, surveyID, voter)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	now := time.Now().UTC()
	r := &Response{ID: newID(12), Voter: voter, Created: now, Updated: now}
	if prev != nil {
		r.ID, r.Created = prev.ID, prev.Created
	}
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
	// choices can shrink between submissions, so it is deleted and reinserted.
	// seen only ever grows: r.Seen is already the union with prev.Seen, so
	// INSERT OR IGNORE against the primary key just no-ops the rows that were
	// already there, with no delete needed.
	if _, err := tx.Exec(`DELETE FROM choices WHERE response_id = ?`, r.ID); err != nil {
		return nil, err
	}
	for _, id := range r.Choices {
		if _, err := tx.Exec(
			`INSERT INTO choices (response_id, option_id) VALUES (?, ?)`, r.ID, id); err != nil {
			return nil, err
		}
	}
	for _, id := range r.Seen {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO seen (response_id, option_id) VALUES (?, ?)`, r.ID, id); err != nil {
			return nil, err
		}
	}
	return r.Clone(), nil
}

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

const (
	maxWriteInsPerVoter = 5
	// A separate, per-survey ceiling on unmoderated suggestions. Without it a
	// single person could fill a survey's whole 500-option budget with pending
	// write-ins, after which editors cannot add options and nobody else can
	// suggest one.
	maxPendingPerSurvey = 50
)

// AddWriteIn records a proposed option and the submitter's vote for it in one
// transaction. The option is pending, so it neither appears on anyone else's
// ballot nor counts in the tally until a moderator approves it. Their existing
// selections and comment are carried forward, so proposing an option does not
// discard the rest of their ballot.
func (s *Store) AddWriteIn(surveyID, voter, text string) (string, error) {
	return s.addWriteIn(surveyID, voter, text, false)
}

// PreviewWriteIn is AddWriteIn for an editor trying out a draft survey.
func (s *Store) PreviewWriteIn(surveyID, voter, text string) (string, error) {
	return s.addWriteIn(surveyID, voter, text, true)
}

func (s *Store) addWriteIn(surveyID, voter, text string, preview bool) (string, error) {
	var newOpt string
	err := s.tx(func(tx *sql.Tx) error {
		sv, err := loadSurvey(tx, surveyID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("survey %q: %w", surveyID, ErrNotFound)
		} else if err != nil {
			return err
		}
		if !sv.AllowWriteIn {
			return fmt.Errorf("this survey does not accept write-ins")
		}
		if !sv.AcceptingFrom(time.Now(), preview) {
			return fmt.Errorf("survey is not accepting responses")
		}

		prev, err := loadResponse(tx, surveyID, voter)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if n := countPending(sv, prev); n >= maxWriteInsPerVoter {
			return fmt.Errorf("you already have %d suggestions awaiting review", n)
		}
		if n := len(sv.PendingOptions()); n >= maxPendingPerSurvey {
			return fmt.Errorf("this survey already has %d suggestions awaiting review; "+
				"try again once a moderator has worked through them", n)
		}

		if newOpt, err = AddOption(sv, text, OptPending, "writein"); err != nil {
			return err
		}
		sv.Updated = time.Now().UTC()
		if err := saveSurvey(tx, sv); err != nil {
			return err
		}

		var choices []string
		var comment string
		if prev != nil {
			choices, comment = append([]string(nil), prev.Choices...), prev.Comment
		}
		_, err = saveResponseTx(tx, surveyID, voter, append(choices, newOpt), comment, newOpt, preview)
		return err
	})
	if err != nil {
		return "", err
	}
	return newOpt, nil
}

// countPending reports how many of a respondent's selections are still awaiting
// moderation.
func countPending(sv *Survey, r *Response) int {
	if sv == nil || r == nil {
		return 0
	}
	n := 0
	for _, id := range r.Choices {
		if o, ok := sv.Option(id); ok && o.Status == OptPending {
			n++
		}
	}
	return n
}

// ResponseFor returns the answer this voter previously gave, if any.
func (s *Store) ResponseFor(surveyID, voter string) (*Response, bool) {
	r, err := loadResponse(s.db, surveyID, voter)
	if err != nil {
		return nil, false
	}
	return r, true
}

// Responses returns every respondent's current answer, oldest first.
func (s *Store) Responses(surveyID string) []*Response {
	rows, err := s.db.Query(
		`SELECT id, voter, comment, created, updated FROM responses
		 WHERE survey_id = ? ORDER BY created, id`, surveyID)
	if err != nil {
		slog.Error("could not read responses", "survey", surveyID, "err", err)
		return nil
	}
	defer rows.Close()
	var out []*Response
	for rows.Next() {
		var r Response
		var created, updated string
		if err := rows.Scan(&r.ID, &r.Voter, &r.Comment, &created, &updated); err != nil {
			return out
		}
		r.Created, r.Updated = goTime(created), goTime(updated)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return out
	}
	byID := make(map[string]*Response, len(out))
	for _, r := range out {
		byID[r.ID] = r
	}
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

// Comments returns the responses that carry a comment, oldest first. It loads
// no choices and no seen rows: the admin page shows the comment and its time
// and nothing else, and the full Responses walk is what the tally cache exists
// to avoid repeating.
func (s *Store) Comments(surveyID string) []*Response {
	rows, err := s.db.Query(
		`SELECT id, voter, comment, created, updated FROM responses
		 WHERE survey_id = ? AND comment != '' ORDER BY created, id`, surveyID)
	if err != nil {
		slog.Error("could not read comments", "survey", surveyID, "err", err)
		return nil
	}
	defer rows.Close()
	var out []*Response
	for rows.Next() {
		var r Response
		var created, updated string
		if err := rows.Scan(&r.ID, &r.Voter, &r.Comment, &created, &updated); err != nil {
			slog.Error("could not read comments", "survey", surveyID, "err", err)
			return out
		}
		r.Created, r.Updated = goTime(created), goTime(updated)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		slog.Error("could not read comments", "survey", surveyID, "err", err)
	}
	return out
}

// Count reports how many people have responded to a survey.
func (s *Store) Count(surveyID string) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM responses WHERE survey_id = ?`, surveyID).Scan(&n)
	return n
}

// ClearResponses discards every response to a survey, returning how many people
// had answered. Irreversible.
func (s *Store) ClearResponses(surveyID string) (int, error) {
	var n int
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		n, err = clearResponsesTx(tx, surveyID)
		return err
	})
	return n, err
}

func clearResponsesTx(tx *sql.Tx, surveyID string) (int, error) {
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM surveys WHERE id = ?`, surveyID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, fmt.Errorf("survey %q: %w", surveyID, ErrNotFound)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM responses WHERE survey_id = ?`, surveyID).Scan(&n); err != nil {
		return 0, err
	}
	// choices and seen go too, through ON DELETE CASCADE.
	if _, err := tx.Exec(`DELETE FROM responses WHERE survey_id = ?`, surveyID); err != nil {
		return 0, err
	}
	return n, nil
}

// Publish moves a survey to the open state. The first time it does so, any
// responses recorded while it was a draft are discarded: only an editor
// previewing it could have produced them, so they are test data. It returns how
// many were discarded.
func (s *Store) Publish(surveyID string) (discarded int, err error) {
	err = s.tx(func(tx *sql.Tx) error {
		sv, err := loadSurvey(tx, surveyID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("survey %q: %w", surveyID, ErrNotFound)
		} else if err != nil {
			return err
		}
		now := time.Now().UTC()
		first := sv.FirstOpenedAt.IsZero()
		if first {
			sv.FirstOpenedAt = now
		}
		sv.State = StateOpen
		// Reopening a survey whose scheduled close has passed would otherwise
		// take effect for zero seconds.
		if !sv.CloseAt.IsZero() && !now.Before(sv.CloseAt) {
			sv.CloseAt = time.Time{}
		}
		sv.Updated = now
		if err := saveSurvey(tx, sv); err != nil {
			return err
		}
		if first {
			discarded, err = clearResponsesTx(tx, surveyID)
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return discarded, nil
}

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

// Tally counts votes per approved option, following merges, and how many
// respondents were shown each one. Respondents is the number of people who
// answered, which is the denominator for Percent: options are not mutually
// exclusive, so percentages do not sum to 100.
//
// The result is cached. This is called on every ballot load of a survey that
// shows results to respondents, and computing it walks every response, every
// choice and every seen row. Before the seen set existed, walking only
// responses and choices was measured at 16ms and 2.7MB at a thousand
// respondents, growing linearly, against 0.65ms for the same page without; a
// seen set runs several times the size of a choice set, so both figures are
// higher now. The cache is keyed on a counter that every committed write
// bumps, so a stale entry is never readable rather than being evicted by
// remembering to.
func (s *Store) Tally(surveyID string) (results []Result, respondents int) {
	gen := s.gen.Load()

	s.tallyMu.Lock()
	if s.tallyGen == gen && s.tallyCache != nil {
		if c, ok := s.tallyCache[surveyID]; ok {
			s.tallyMu.Unlock()
			// A copy, so a caller sorting or editing the slice cannot corrupt
			// what every other request will be served.
			return append([]Result(nil), c.results...), c.respondents
		}
	}
	s.tallyMu.Unlock()

	results, respondents = s.computeTally(surveyID)

	s.tallyMu.Lock()
	if s.tallyGen != gen {
		// A write landed while this was being computed. Drop the whole map:
		// resetting rather than deleting one key keeps memory bounded to the
		// surveys actually being read, with no eviction policy to tune.
		s.tallyCache, s.tallyGen = map[string]cachedTally{}, gen
	}
	if s.tallyCache == nil {
		s.tallyCache = map[string]cachedTally{}
	}
	// Only cache if nothing has been committed since the read began; otherwise
	// this result is already stale and must not be stored.
	if s.gen.Load() == gen {
		s.tallyGen = gen
		s.tallyCache[surveyID] = cachedTally{results: append([]Result(nil), results...), respondents: respondents}
	}
	s.tallyMu.Unlock()
	return results, respondents
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

func sortResultsByVotes(results []Result) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Votes > results[j].Votes })
}
