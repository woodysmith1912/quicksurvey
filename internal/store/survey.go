package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// Survey states. A survey only accepts responses while it is open.
const (
	StateDraft  = "draft"
	StateOpen   = "open"
	StateClosed = "closed"
)

// Option statuses.
//
// Options are never deleted from a survey, because responses reference them by
// ID and the response log is append-only. Removing or merging an option is a
// status change, which keeps historical responses interpretable.
const (
	OptApproved = "approved" // counts in the tally, shown to respondents
	OptPending  = "pending"  // write-in awaiting moderation
	OptRejected = "rejected" // write-in refused
	OptRemoved  = "removed"  // editor deleted it after votes existed
	OptMerged   = "merged"   // folded into MergedInto; votes transfer there
)

// SurveyType identifies the question format. Only one exists today; the field
// is here so a second format does not require migrating stored surveys.
const TypeThumbsUp = "thumbsup"

// Option is one thing a respondent can thumbs-up.
type Option struct {
	ID         string    `json:"id"`
	Text       string    `json:"text"`
	Status     string    `json:"status"`
	Source     string    `json:"source"`                // "editor" or "writein"
	MergedInto string    `json:"merged_into,omitempty"` // set when Status == OptMerged
	Created    time.Time `json:"created"`
}

// Survey is the definition respondents see. It carries no response data.
type Survey struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Options      []Option `json:"options"`
	State        string   `json:"state"`
	ShowResults  bool     `json:"show_results"` // respondents see the tally
	AllowWriteIn bool     `json:"allow_write_in"`
	AllowComment bool     `json:"allow_comment"`
	// NoRandomize turns off per-respondent shuffling of the options.
	//
	// Stored inverted, and read through Randomize, so that the zero value
	// means "shuffle". Randomising is the default because the order options
	// are listed measurably biases which ones get picked, and a survey stored
	// before this field existed should get the better behaviour too.
	NoRandomize bool `json:"no_randomize,omitempty"`
	// CloseAt, if non-zero, is the instant after which the survey stops
	// accepting responses without anyone having to flip the state. Stored in
	// UTC; entered and displayed in the server's configured location.
	CloseAt time.Time `json:"close_at,omitzero"`
	// FirstOpenedAt records when the survey was first published. Until it is
	// set, any responses present can only have come from an editor testing the
	// draft, which is what makes discarding them at publication safe.
	FirstOpenedAt time.Time `json:"first_opened_at,omitzero"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
}

// Clone returns a deep copy, so callers can read a survey without holding the
// store lock and without racing a concurrent edit.
func (s *Survey) Clone() *Survey {
	c := *s
	c.Options = append([]Option(nil), s.Options...)
	return &c
}

// Accepting reports whether the survey takes responses from the public at
// time now.
func (s *Survey) Accepting(now time.Time) bool {
	if s.State != StateOpen {
		return false
	}
	return s.CloseAt.IsZero() || now.Before(s.CloseAt)
}

// AcceptingFrom is Accepting, extended for an editor previewing the survey: a
// draft takes responses from them so they can try it before publishing it.
// Everything else — a closed survey, a passed close time — still applies.
func (s *Survey) AcceptingFrom(now time.Time, previewer bool) bool {
	if previewer && s.State == StateDraft {
		return true
	}
	return s.Accepting(now)
}

// ClosedByClock reports whether an otherwise-open survey has passed its
// scheduled close time. Used to explain the difference to editors.
func (s *Survey) ClosedByClock(now time.Time) bool {
	return s.State == StateOpen && !s.CloseAt.IsZero() && !now.Before(s.CloseAt)
}

// Randomize reports whether respondents should see the options shuffled.
func (s *Survey) Randomize() bool { return !s.NoRandomize }

// Option looks up an option by ID.
func (s *Survey) Option(id string) (Option, bool) {
	for _, o := range s.Options {
		if o.ID == id {
			return o, true
		}
	}
	return Option{}, false
}

// Resolve follows merge chains to the option a vote should ultimately count
// for, and reports whether that option is approved. A malformed or cyclic chain
// resolves to not-counted rather than looping.
func (s *Survey) Resolve(id string) (string, bool) {
	for range len(s.Options) + 1 {
		o, ok := s.Option(id)
		if !ok {
			return id, false
		}
		if o.Status == OptMerged && o.MergedInto != "" {
			id = o.MergedInto
			continue
		}
		return o.ID, o.Status == OptApproved
	}
	return id, false
}

// Ballot returns the options to show a respondent in the order the editor
// wrote them: approved ones first, followed by this respondent's own pending
// write-ins. BallotFor is what respondents actually get.
func (s *Survey) Ballot(pending []string) []Option {
	out := make([]Option, 0, len(s.Options))
	for _, o := range s.Options {
		if o.Status == OptApproved {
			out = append(out, o)
		}
	}
	for _, id := range pending {
		if o, ok := s.Option(id); ok && o.Status == OptPending {
			out = append(out, o)
		}
	}
	return out
}

// BallotFor is Ballot, shuffled for one respondent when the survey randomises.
//
// The shuffle is derived from seed — in practice the respondent's per-survey
// identifier — rather than being drawn fresh on each render. That matters:
// someone who reloads the page, or comes back to change their answer, must see
// the same order, or their existing ticks appear to move around. Different
// people get different orders; the same person always gets theirs.
//
// A respondent's own pending write-ins stay at the end, where they were added.
func (s *Survey) BallotFor(pending []string, seed string) []Option {
	approved := s.Ballot(nil)
	if s.Randomize() && len(approved) > 1 {
		r := rand.New(rand.NewChaCha8(sha256.Sum256([]byte(seed))))
		r.Shuffle(len(approved), func(i, j int) {
			approved[i], approved[j] = approved[j], approved[i]
		})
	}
	for _, id := range pending {
		if o, ok := s.Option(id); ok && o.Status == OptPending {
			approved = append(approved, o)
		}
	}
	return approved
}

// PendingOptions returns write-ins awaiting moderation, oldest first.
func (s *Survey) PendingOptions() []Option {
	var out []Option
	for _, o := range s.Options {
		if o.Status == OptPending {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// --- persistence ----------------------------------------------------------

const surveyColumns = `id, type, title, description, state, show_results,
	allow_write_in, allow_comment, no_randomize, close_at, first_opened_at, created, updated`

func scanSurvey(row rowScanner) (*Survey, error) {
	var sv Survey
	var created, updated string
	var closeAt, firstOpened sql.NullString
	if err := row.Scan(&sv.ID, &sv.Type, &sv.Title, &sv.Description, &sv.State,
		&sv.ShowResults, &sv.AllowWriteIn, &sv.AllowComment, &sv.NoRandomize,
		&closeAt, &firstOpened, &created, &updated); err != nil {
		return nil, err
	}
	sv.CloseAt, sv.FirstOpenedAt = goTimePtr(closeAt), goTimePtr(firstOpened)
	sv.Created, sv.Updated = goTime(created), goTime(updated)
	return &sv, nil
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// loadOptions fills in a survey's options, in the editor's order.
func loadOptions(q queryer, sv *Survey) error {
	rows, err := q.Query(
		`SELECT id, text, status, source, merged_into, created
		 FROM options WHERE survey_id = ? ORDER BY position`, sv.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var o Option
		var created string
		if err := rows.Scan(&o.ID, &o.Text, &o.Status, &o.Source, &o.MergedInto, &created); err != nil {
			return err
		}
		o.Created = goTime(created)
		sv.Options = append(sv.Options, o)
	}
	return rows.Err()
}

func loadSurvey(q queryer, id string) (*Survey, error) {
	sv, err := scanSurvey(q.QueryRow(`SELECT `+surveyColumns+` FROM surveys WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := loadOptions(q, sv); err != nil {
		return nil, err
	}
	return sv, nil
}

// saveSurvey writes a survey and its options. Options are upserted by ID rather
// than replaced, because responses reference them and an ID must never be
// reused for different text.
func saveSurvey(tx *sql.Tx, sv *Survey) error {
	if _, err := tx.Exec(
		`INSERT INTO surveys (`+surveyColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   type=excluded.type, title=excluded.title, description=excluded.description,
		   state=excluded.state, show_results=excluded.show_results,
		   allow_write_in=excluded.allow_write_in, allow_comment=excluded.allow_comment,
		   no_randomize=excluded.no_randomize, close_at=excluded.close_at,
		   first_opened_at=excluded.first_opened_at, updated=excluded.updated`,
		sv.ID, sv.Type, sv.Title, sv.Description, sv.State, sv.ShowResults,
		sv.AllowWriteIn, sv.AllowComment, sv.NoRandomize,
		dbTimePtr(sv.CloseAt), dbTimePtr(sv.FirstOpenedAt),
		dbTime(sv.Created), dbTime(sv.Updated)); err != nil {
		return err
	}
	for i, o := range sv.Options {
		if _, err := tx.Exec(
			`INSERT INTO options (id, survey_id, position, text, status, source, merged_into, created)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			   position=excluded.position, text=excluded.text, status=excluded.status,
			   source=excluded.source, merged_into=excluded.merged_into`,
			o.ID, sv.ID, i, o.Text, o.Status, o.Source, o.MergedInto, dbTime(o.Created)); err != nil {
			return err
		}
	}
	return nil
}

// CreateSurvey stores a new survey in the draft state.
func (s *Store) CreateSurvey(title, description string, optionTexts []string) (*Survey, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("survey needs a title")
	}
	now := time.Now().UTC()
	sv := &Survey{
		ID:           newID(10),
		Type:         TypeThumbsUp,
		Title:        title,
		Description:  strings.TrimSpace(description),
		State:        StateDraft,
		AllowWriteIn: true,
		AllowComment: true,
		Created:      now,
		Updated:      now,
	}
	for _, t := range optionTexts {
		if t = strings.TrimSpace(t); t != "" {
			sv.Options = append(sv.Options, Option{
				ID: newID(6), Text: t, Status: OptApproved, Source: "editor", Created: now,
			})
		}
	}
	if err := s.tx(func(tx *sql.Tx) error { return saveSurvey(tx, sv) }); err != nil {
		return nil, err
	}
	return sv.Clone(), nil
}

// Survey returns a copy of the named survey.
func (s *Store) Survey(id string) (*Survey, bool) {
	sv, err := loadSurvey(s.db, id)
	if err != nil {
		return nil, false
	}
	return sv, true
}

// Surveys returns every survey, newest first.
func (s *Store) Surveys() []*Survey {
	rows, err := s.db.Query(`SELECT ` + surveyColumns + ` FROM surveys ORDER BY created DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Survey
	for rows.Next() {
		sv, err := scanSurvey(rows)
		if err != nil {
			return out
		}
		out = append(out, sv)
	}
	if err := rows.Err(); err != nil {
		return out
	}
	for _, sv := range out {
		if err := loadOptions(s.db, sv); err != nil {
			return out
		}
	}
	return out
}

// UpdateSurvey applies fn to the stored survey inside a transaction and
// persists the result. fn must not call back into the store.
func (s *Store) UpdateSurvey(id string, fn func(*Survey) error) (*Survey, error) {
	var out *Survey
	err := s.tx(func(tx *sql.Tx) error {
		sv, err := loadSurvey(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("survey %q: %w", id, ErrNotFound)
		} else if err != nil {
			return err
		}
		if err := fn(sv); err != nil {
			return err
		}
		sv.Updated = time.Now().UTC()
		if err := saveSurvey(tx, sv); err != nil {
			return err
		}
		out = sv.Clone()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteSurvey removes a survey and every response to it, irreversibly. The
// options and responses go with it through ON DELETE CASCADE.
func (s *Store) DeleteSurvey(id string) error {
	return s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM surveys WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("survey %q: %w", id, ErrNotFound)
		}
		return nil
	})
}

// SetOptionText renames an option. Votes are unaffected: they reference the ID.
func SetOptionText(sv *Survey, id, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("option text must not be empty")
	}
	if len(text) > MaxOptionText {
		return fmt.Errorf("option text must be %d characters or fewer", MaxOptionText)
	}
	for i := range sv.Options {
		if sv.Options[i].ID == id {
			sv.Options[i].Text = text
			return nil
		}
	}
	return fmt.Errorf("option %q: %w", id, ErrNotFound)
}

// SetOptionStatus changes an option's moderation status, optionally merging it
// into another option. Merging transfers votes without touching the response
// log: tallies follow the merge pointer.
func SetOptionStatus(sv *Survey, id, status, mergeInto string) error {
	idx := -1
	for i := range sv.Options {
		if sv.Options[i].ID == id {
			idx = i
		}
	}
	if idx < 0 {
		return fmt.Errorf("option %q: %w", id, ErrNotFound)
	}
	switch status {
	case OptApproved, OptPending, OptRejected, OptRemoved:
		sv.Options[idx].Status = status
		sv.Options[idx].MergedInto = ""
	case OptMerged:
		if mergeInto == "" || mergeInto == id {
			return fmt.Errorf("merge target must be a different option")
		}
		target, ok := sv.Option(mergeInto)
		if !ok {
			return fmt.Errorf("merge target %q: %w", mergeInto, ErrNotFound)
		}
		if target.Status == OptMerged {
			return fmt.Errorf("cannot merge into an already-merged option")
		}
		sv.Options[idx].Status = OptMerged
		sv.Options[idx].MergedInto = mergeInto
	default:
		return fmt.Errorf("unknown option status %q", status)
	}
	return nil
}

// AddOption appends an option and returns its ID.
func AddOption(sv *Survey, text, status, source string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("option text must not be empty")
	}
	// The form says maxlength=200, but that is a suggestion to a browser. An
	// anonymous write-in reaches this function straight from a POST body, and
	// without a cap here one request can store a megabyte that is then
	// reloaded on every subsequent request to the survey and emitted as a
	// column header in the export.
	if len(text) > MaxOptionText {
		return "", fmt.Errorf("option text must be %d characters or fewer", MaxOptionText)
	}
	if len(sv.Options) >= maxOptions {
		return "", fmt.Errorf("survey already has the maximum of %d options", maxOptions)
	}
	o := Option{ID: newID(6), Text: text, Status: status, Source: source, Created: time.Now().UTC()}
	sv.Options = append(sv.Options, o)
	return o.ID, nil
}

const maxOptions = 500

// MaxOptionText bounds any option's text, wherever it came from.
const MaxOptionText = 200
