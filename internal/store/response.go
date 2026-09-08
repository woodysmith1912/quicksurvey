package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Response is one person's answer to one survey.
//
// It deliberately contains nothing that identifies a human being: no account,
// no IP address, no user agent. Voter is a per-survey pseudonym derived from a
// random browser cookie (see Store.VoterID), which is what makes repeat
// submissions detectable without making respondents identifiable.
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

func (s *Store) responsePath(surveyID string) string {
	return filepath.Join(s.surveyDir(surveyID), "responses.jsonl")
}

// loadResponses replays the append-only log. Later records for the same voter
// supersede earlier ones, which is how vote edits work.
func (s *Store) loadResponses(surveyID string) error {
	byVoter := map[string]*Response{}
	s.responses[surveyID] = byVoter

	f, err := os.Open(s.responsePath(surveyID))
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for line := 1; sc.Scan(); line++ {
		b := strings.TrimSpace(sc.Text())
		if b == "" {
			continue
		}
		var r Response
		if err := json.Unmarshal([]byte(b), &r); err != nil {
			// A torn final line is the expected failure after a hard kill.
			// Skipping it loses one response; refusing to start loses all.
			fmt.Fprintf(os.Stderr, "quicksurvey: survey %s: skipping unreadable response at line %d: %v\n", surveyID, line, err)
			continue
		}
		byVoter[r.Voter] = &r
	}
	return sc.Err()
}

// appendResponse must be called with s.mu held.
func (s *Store) appendResponse(surveyID string, r *Response) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.responsePath(surveyID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// MaxComment bounds the free-text field, so one respondent cannot fill the disk.
const MaxComment = 2000

// SaveResponse records a respondent's choices, replacing any answer they
// previously gave. It returns the stored response.
func (s *Store) SaveResponse(surveyID, voter string, choices []string, comment string) (*Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveResponseLocked(surveyID, voter, choices, comment, "", false)
}

// PreviewResponse is SaveResponse for an editor trying out a survey that is
// still a draft. Such responses are real records, and are discarded when the
// survey is published for the first time.
func (s *Store) PreviewResponse(surveyID, voter string, choices []string, comment string) (*Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveResponseLocked(surveyID, voter, choices, comment, "", true)
}

// saveResponseLocked is the body of SaveResponse. It must be called with s.mu
// held, which is what lets AddWriteIn create an option and vote for it in one
// atomic step.
//
// Choices are filtered against the survey: a respondent may select approved
// options, pending write-ins they already had selected, and allowPending, which
// is the option they are proposing in this same request. Anything else is
// dropped rather than rejected, so a stale form does not lose the whole ballot.
func (s *Store) saveResponseLocked(surveyID, voter string, choices []string, comment, allowPending string, preview bool) (*Response, error) {
	sv, ok := s.surveys[surveyID]
	if !ok {
		return nil, fmt.Errorf("survey %q: %w", surveyID, ErrNotFound)
	}
	if !sv.AcceptingFrom(time.Now(), preview) {
		return nil, fmt.Errorf("survey is not accepting responses")
	}
	byVoter := s.responses[surveyID]
	if byVoter == nil {
		byVoter = map[string]*Response{}
		s.responses[surveyID] = byVoter
	}
	prev := byVoter[voter]

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
			// A pending write-in is selectable only by the person who
			// proposed it: either they are proposing it right now, or their
			// previous response already references it.
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

	if err := s.appendResponse(surveyID, r); err != nil {
		return nil, err
	}
	byVoter[voter] = r
	return r.Clone(), nil
}

// AddWriteIn records a proposed option and the submitter's vote for it in one
// step. The option is pending, so it neither appears on anyone else's ballot nor
// counts in the tally until a moderator approves it. Their existing selections
// and comment are carried forward, so proposing an option does not discard the
// rest of their ballot.
//
// Both writes happen under one lock, and the option is rolled back if the
// response cannot be stored, so a failure cannot leave an orphan in the
// moderation queue.
func (s *Store) AddWriteIn(surveyID, voter, text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addWriteInLocked(surveyID, voter, text, false)
}

// PreviewWriteIn is AddWriteIn for an editor trying out a draft survey.
func (s *Store) PreviewWriteIn(surveyID, voter, text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addWriteInLocked(surveyID, voter, text, true)
}

func (s *Store) addWriteInLocked(surveyID, voter, text string, preview bool) (string, error) {
	sv, ok := s.surveys[surveyID]
	if !ok {
		return "", fmt.Errorf("survey %q: %w", surveyID, ErrNotFound)
	}
	if !sv.AllowWriteIn {
		return "", fmt.Errorf("this survey does not accept write-ins")
	}
	if !sv.AcceptingFrom(time.Now(), preview) {
		return "", fmt.Errorf("survey is not accepting responses")
	}
	prev := s.responses[surveyID][voter]
	if n := countPending(sv, prev); n >= maxWriteInsPerVoter {
		return "", fmt.Errorf("you already have %d suggestions awaiting review", n)
	}

	draft := sv.Clone()
	newOpt, err := AddOption(draft, text, OptPending, "writein")
	if err != nil {
		return "", err
	}
	draft.Updated = time.Now().UTC()
	if err := s.saveSurvey(draft); err != nil {
		return "", err
	}
	s.surveys[surveyID] = draft

	var choices []string
	var comment string
	if prev != nil {
		choices, comment = append([]string(nil), prev.Choices...), prev.Comment
	}
	if _, err := s.saveResponseLocked(surveyID, voter, append(choices, newOpt), comment, newOpt, preview); err != nil {
		s.surveys[surveyID] = sv // roll back rather than leave an unvoted-for suggestion
		if saveErr := s.saveSurvey(sv); saveErr != nil {
			return "", fmt.Errorf("%w (and rolling back the suggestion failed: %v)", err, saveErr)
		}
		return "", err
	}
	return newOpt, nil
}

const maxWriteInsPerVoter = 5

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

// ClearResponses discards every response to a survey, returning how many
// people had answered. Irreversible: the log file is removed, not truncated.
func (s *Store) ClearResponses(surveyID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clearResponsesLocked(surveyID)
}

func (s *Store) clearResponsesLocked(surveyID string) (int, error) {
	if _, ok := s.surveys[surveyID]; !ok {
		return 0, fmt.Errorf("survey %q: %w", surveyID, ErrNotFound)
	}
	n := len(s.responses[surveyID])
	if err := os.Remove(s.responsePath(surveyID)); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	s.responses[surveyID] = map[string]*Response{}
	return n, nil
}

// Publish moves a survey to the open state. The first time it does so, any
// responses recorded while it was a draft are discarded: only an editor
// previewing it could have produced them, so they are test data. It returns how
// many were discarded.
func (s *Store) Publish(surveyID string) (discarded int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sv, ok := s.surveys[surveyID]
	if !ok {
		return 0, fmt.Errorf("survey %q: %w", surveyID, ErrNotFound)
	}
	draft := sv.Clone()
	now := time.Now().UTC()
	first := draft.FirstOpenedAt.IsZero()
	if first {
		draft.FirstOpenedAt = now
	}
	draft.State = StateOpen
	// Reopening a survey whose scheduled close has passed would otherwise take
	// effect for zero seconds.
	if !draft.CloseAt.IsZero() && !now.Before(draft.CloseAt) {
		draft.CloseAt = time.Time{}
	}
	draft.Updated = now
	if err := s.saveSurvey(draft); err != nil {
		return 0, err
	}
	s.surveys[surveyID] = draft

	if first {
		if discarded, err = s.clearResponsesLocked(surveyID); err != nil {
			return 0, err
		}
	}
	return discarded, nil
}

// ResponseFor returns a copy of the answer this voter previously gave, if any.
func (s *Store) ResponseFor(surveyID, voter string) (*Response, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.responses[surveyID][voter]
	if !ok {
		return nil, false
	}
	return r.Clone(), true
}

// Responses returns the current answer of every respondent, oldest first.
func (s *Store) Responses(surveyID string) []*Response {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Response, 0, len(s.responses[surveyID]))
	for _, r := range s.responses[surveyID] {
		out = append(out, r.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Count reports how many people have responded to a survey.
func (s *Store) Count(surveyID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.responses[surveyID])
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	sv := s.surveys[surveyID]
	if sv == nil {
		return nil, 0
	}
	counts := map[string]int{}
	for _, r := range s.responses[surveyID] {
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
	sort.SliceStable(results, func(i, j int) bool { return results[i].Votes > results[j].Votes })
	return results, respondents
}
