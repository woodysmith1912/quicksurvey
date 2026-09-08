package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// Ballot returns the options to show a respondent: approved ones in order,
// followed by this respondent's own pending write-ins.
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

func (s *Store) surveyDir(id string) string  { return filepath.Join(s.dir, "surveys", id) }
func (s *Store) surveyPath(id string) string { return filepath.Join(s.surveyDir(id), "survey.json") }

func (s *Store) loadSurveys() error {
	entries, err := os.ReadDir(filepath.Join(s.dir, "surveys"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(s.surveyPath(e.Name()))
		if os.IsNotExist(err) {
			continue // directory without a definition: ignore
		} else if err != nil {
			return err
		}
		var sv Survey
		if err := json.Unmarshal(b, &sv); err != nil {
			return fmt.Errorf("survey %s: %w", e.Name(), err)
		}
		s.surveys[sv.ID] = &sv
		if err := s.loadResponses(sv.ID); err != nil {
			return err
		}
	}
	return nil
}

// saveSurvey must be called with s.mu held.
func (s *Store) saveSurvey(sv *Survey) error {
	if err := os.MkdirAll(s.surveyDir(sv.ID), 0o700); err != nil {
		return err
	}
	return writeJSONAtomic(s.surveyPath(sv.ID), sv, 0o600)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.surveys[sv.ID]; ok {
		return nil, ErrExists
	}
	if err := s.saveSurvey(sv); err != nil {
		return nil, err
	}
	s.surveys[sv.ID] = sv
	s.responses[sv.ID] = map[string]*Response{}
	return sv.Clone(), nil
}

// Survey returns a copy of the named survey.
func (s *Store) Survey(id string) (*Survey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sv, ok := s.surveys[id]
	if !ok {
		return nil, false
	}
	return sv.Clone(), true
}

// Surveys returns copies of every survey, newest first.
func (s *Store) Surveys() []*Survey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Survey, 0, len(s.surveys))
	for _, sv := range s.surveys {
		out = append(out, sv.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// UpdateSurvey applies fn to the stored survey under lock and persists the
// result. fn must not call back into the store.
func (s *Store) UpdateSurvey(id string, fn func(*Survey) error) (*Survey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sv, ok := s.surveys[id]
	if !ok {
		return nil, fmt.Errorf("survey %q: %w", id, ErrNotFound)
	}
	draft := sv.Clone()
	if err := fn(draft); err != nil {
		return nil, err
	}
	draft.Updated = time.Now().UTC()
	if err := s.saveSurvey(draft); err != nil {
		return nil, err
	}
	s.surveys[id] = draft
	return draft.Clone(), nil
}

// DeleteSurvey removes a survey and every response to it, irreversibly.
func (s *Store) DeleteSurvey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.surveys[id]; !ok {
		return fmt.Errorf("survey %q: %w", id, ErrNotFound)
	}
	if err := os.RemoveAll(s.surveyDir(id)); err != nil {
		return err
	}
	delete(s.surveys, id)
	delete(s.responses, id)
	return nil
}

// SetOptionText renames an option. Votes are unaffected: they reference the ID.
func SetOptionText(sv *Survey, id, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("option text must not be empty")
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
	if len(sv.Options) >= maxOptions {
		return "", fmt.Errorf("survey already has the maximum of %d options", maxOptions)
	}
	o := Option{ID: newID(6), Text: text, Status: status, Source: source, Created: time.Now().UTC()}
	sv.Options = append(sv.Options, o)
	return o.ID, nil
}

const maxOptions = 500
