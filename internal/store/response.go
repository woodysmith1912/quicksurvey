package store

import (
	"database/sql"
	"errors"
	"fmt"
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
// There is one row per respondent per survey. A second submission replaces the
// first rather than adding to it, which is what stops anyone inflating a count
// by resubmitting. Superseded answers are not retained.
type Response struct {
	ID      string    `json:"id"`
	Voter   string    `json:"voter"`
	Choices []string  `json:"choices"` // option IDs
	Comment string    `json:"comment,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Clone returns a deep copy.
func (r *Response) Clone() *Response {
	c := *r
	c.Choices = append([]string(nil), r.Choices...)
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

// saveResponseTx is the body of SaveResponse, inside a caller's transaction.
//
// Choices are filtered against the survey: a respondent may select approved
// options, pending write-ins they already had selected, and allowPending, which
// is the option they are proposing in this same request. Anything else is
// dropped rather than rejected, so a stale form does not lose the whole ballot.
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
	seen := map[string]bool{}
	for _, id := range choices {
		o, ok := sv.Option(id)
		if !ok || seen[id] {
			continue
		}
		switch o.Status {
		case OptApproved:
		case OptPending:
			// A pending write-in is selectable only by the person who proposed
			// it: either they are proposing it right now, or their previous
			// response already references it.
			if id != allowPending && (prev == nil || !prev.Chose(id)) {
				continue
			}
		default:
			continue
		}
		seen[id] = true
		r.Choices = append(r.Choices, id)
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
	if _, err := tx.Exec(`DELETE FROM choices WHERE response_id = ?`, r.ID); err != nil {
		return nil, err
	}
	for _, id := range r.Choices {
		if _, err := tx.Exec(
			`INSERT INTO choices (response_id, option_id) VALUES (?, ?)`, r.ID, id); err != nil {
			return nil, err
		}
	}
	return r.Clone(), nil
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
	rows, err := q.Query(`SELECT option_id FROM choices WHERE response_id = ?`, r.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		r.Choices = append(r.Choices, id)
	}
	return &r, rows.Err()
}

const maxWriteInsPerVoter = 5

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
	crows, err := s.db.Query(
		`SELECT c.response_id, c.option_id FROM choices c
		 JOIN responses r ON r.id = c.response_id WHERE r.survey_id = ?`, surveyID)
	if err != nil {
		return out
	}
	defer crows.Close()
	for crows.Next() {
		var rid, oid string
		if err := crows.Scan(&rid, &oid); err != nil {
			return out
		}
		if r, ok := byID[rid]; ok {
			r.Choices = append(r.Choices, oid)
		}
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
	// choices go too, through ON DELETE CASCADE.
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
	Percent float64 // share of respondents who selected it
}

// Tally counts votes per approved option, following merges. Respondents is the
// number of people who answered, which is the denominator for Percent: options
// are not mutually exclusive, so percentages do not sum to 100.
func (s *Store) Tally(surveyID string) (results []Result, respondents int) {
	sv, ok := s.Survey(surveyID)
	if !ok {
		return nil, 0
	}
	counts := map[string]int{}
	for _, r := range s.Responses(surveyID) {
		respondents++
		counted := map[string]bool{}
		for _, id := range r.Choices {
			// Two merged options can resolve to the same target; one person
			// must still only count once for it.
			if target, ok := sv.Resolve(id); ok && !counted[target] {
				counted[target] = true
				counts[target]++
			}
		}
	}
	for _, o := range sv.Options {
		if o.Status != OptApproved {
			continue
		}
		res := Result{Option: o, Votes: counts[o.ID]}
		if respondents > 0 {
			res.Percent = 100 * float64(res.Votes) / float64(respondents)
		}
		results = append(results, res)
	}
	sortResultsByVotes(results)
	return results, respondents
}

func sortResultsByVotes(results []Result) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Votes > results[j].Votes })
}
