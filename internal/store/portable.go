package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// A survey document is the save/restore form of a survey. It comes in two
// shapes, and the difference is not cosmetic.
//
// A definition document carries what the survey asks and how it behaves, and
// nothing that came from a respondent: no responses, no identifiers, no
// publication history. Restoring one always produces a new draft. It is how
// you start a survey from an old one, including somewhere else.
//
// A full backup additionally carries every option — including the ones
// moderation hid — and every response, with the identifiers that bind them
// together. Restoring one reproduces the survey as it was. It is how you put
// back something you are about to delete.
//
// The document deliberately does not reuse Survey. A file people keep on disk
// should not change shape every time an internal field is added — group
// ownership is about to add one — and "a definition carries no responses" is
// worth making structurally true rather than leaving it to whoever remembers
// to blank the fields before writing.
const (
	docFormat  = "quicksurvey.survey"
	docVersion = 1
)

// Document is one saved survey.
type Document struct {
	Format   string    `json:"format"`
	Version  int       `json:"version"`
	Exported time.Time `json:"exported"`
	// SourceID is the survey this came from. In a definition document it is
	// provenance for whoever reads the file and nothing more. In a full backup
	// it is load-bearing: see RestoreSurvey.
	SourceID string       `json:"source_id,omitempty"`
	Survey   DocumentBody `json:"survey"`
	// Responses is present only in a full backup.
	Responses []DocumentResponse `json:"responses,omitempty"`
}

// Full reports whether the document carries response data.
func (d *Document) Full() bool { return d.Responses != nil || d.Survey.State != "" }

// DocumentBody is the survey itself. State and the timestamps below it appear
// only in a full backup; a definition document omits them, because a restored
// definition is a new draft and carrying the original's history would only
// describe a survey that no longer exists.
type DocumentBody struct {
	Type         string           `json:"type"`
	Title        string           `json:"title"`
	Description  string           `json:"description,omitempty"`
	ShowResults  bool             `json:"show_results"`
	AllowWriteIn bool             `json:"allow_write_in"`
	AllowComment bool             `json:"allow_comment"`
	NoRandomize  bool             `json:"no_randomize,omitempty"`
	Options      []DocumentOption `json:"options"`

	State         string    `json:"state,omitempty"`
	CloseAt       time.Time `json:"close_at,omitzero"`
	FirstOpenedAt time.Time `json:"first_opened_at,omitzero"`
	Created       time.Time `json:"created,omitzero"`
	Updated       time.Time `json:"updated,omitzero"`
}

// DocumentOption is one choice. ID, Status, MergedInto and Created appear only
// in a full backup, where responses reference options by ID and a vote for an
// option that moderation later hid must still resolve to something.
type DocumentOption struct {
	Text string `json:"text"`
	// Source is "editor" or "writein", kept so a restored survey still says
	// which options came from respondents rather than from whoever wrote it.
	Source string `json:"source,omitempty"`

	ID         string    `json:"id,omitempty"`
	Status     string    `json:"status,omitempty"`
	MergedInto string    `json:"merged_into,omitempty"`
	Created    time.Time `json:"created,omitzero"`
}

// DocumentResponse is one respondent's answer.
//
// Voter is the derived identifier, not anything that identifies a person: it
// is MAC("voter", surveyID, cookie token), so it can only be recomputed by the
// same instance, for the same survey ID, from the same browser. That is
// exactly why restoring responses requires the original survey ID — see
// RestoreSurvey.
//
// The response's own row ID is not carried. Nothing outside the database
// references it, so a restore mints a fresh one rather than risking a
// collision with a row that already exists.
type DocumentResponse struct {
	Voter   string   `json:"voter"`
	Choices []string `json:"choices"` // option IDs
	// Seen is every option that was on this respondent's ballot at any
	// submission. It has to travel with the choices: Shown is derived from it,
	// the tally asserts Votes <= Shown <= respondents, and a restore that
	// dropped it would put every option back reading "shown to 0" beside a
	// nonzero vote count -- wrong, and unrepairable, because backfillSeen has
	// already run on any database old enough to restore into.
	Seen    []string  `json:"seen"`
	Comment string    `json:"comment,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// SurveyDocument returns a survey's definition, or with responses a full
// backup of it.
//
// In a definition, only approved options travel. Pending and rejected
// write-ins are text a respondent typed that was never put on the ballot, and
// a file described as carrying no responses must not carry them. Merged and
// removed options are moderation state that means nothing without the votes it
// was moderating, and those are not coming either. Approved write-ins do
// travel: once approved an option is on the ballot, and what is on the ballot
// is what the survey asks.
//
// A full backup keeps every option whatever its status, because a stored
// response may reference any of them.
func (s *Store) SurveyDocument(id string, withResponses bool) (*Document, error) {
	sv, ok := s.Survey(id)
	if !ok {
		return nil, fmt.Errorf("no survey with ID %q", id)
	}
	doc := &Document{
		Format:   docFormat,
		Version:  docVersion,
		Exported: time.Now().UTC(),
		SourceID: sv.ID,
		Survey: DocumentBody{
			Type:         sv.Type,
			Title:        sv.Title,
			Description:  sv.Description,
			ShowResults:  sv.ShowResults,
			AllowWriteIn: sv.AllowWriteIn,
			AllowComment: sv.AllowComment,
			NoRandomize:  sv.NoRandomize,
		},
	}
	for _, o := range sv.Options {
		if !withResponses && o.Status != OptApproved {
			continue
		}
		d := DocumentOption{Text: o.Text, Source: o.Source}
		if withResponses {
			d.ID, d.Status, d.MergedInto, d.Created = o.ID, o.Status, o.MergedInto, o.Created
		}
		doc.Survey.Options = append(doc.Survey.Options, d)
	}
	if !withResponses {
		return doc, nil
	}

	// State and the publication history are part of a backup, and leaving them
	// out would be worse than untidy: Publish discards a draft's responses
	// precisely when FirstOpenedAt is unset, so a backup restored as a fresh
	// draft would lose every restored response the next time it was published.
	doc.Survey.State = sv.State
	doc.Survey.CloseAt = sv.CloseAt
	doc.Survey.FirstOpenedAt = sv.FirstOpenedAt
	doc.Survey.Created = sv.Created
	doc.Survey.Updated = sv.Updated

	doc.Responses = []DocumentResponse{}
	for _, r := range s.Responses(sv.ID) {
		doc.Responses = append(doc.Responses, DocumentResponse{
			Voter:   r.Voter,
			Choices: append([]string(nil), r.Choices...),
			Seen:    append([]string(nil), r.Seen...),
			Comment: r.Comment,
			Created: r.Created,
			Updated: r.Updated,
		})
	}
	return doc, nil
}

// RestoreSurvey writes a document back into the store as a survey.
//
// With keepID false it is always a new survey and always a draft: fresh
// identifiers throughout, because reusing the original survey ID would
// resurrect respondent links and reusing option IDs would let votes recorded
// against the original attach to the copy.
//
// With keepID true it goes back under the identifiers it was saved with, and
// is refused if a survey already holds that ID. That is what makes duplicate
// rejection survive a restore: a respondent's stored identifier is derived
// from the survey ID, so the same browser only recomputes the same identifier
// — and so only replaces its earlier answer rather than adding a second one —
// if the survey ID is the one it voted under. The instance HMAC key has to
// match too, which is automatic on the instance the backup came from and is
// why restoring elsewhere needs that key carried across as well.
//
// Responses therefore require keepID. Restoring them under a fresh survey ID
// would produce rows no respondent can ever match, silently turning the
// duplicate rejection they exist to support into a survey anyone can answer
// twice.
func (s *Store) RestoreSurvey(doc *Document, keepID bool) (*Survey, error) {
	if doc == nil {
		return nil, fmt.Errorf("no document")
	}
	if doc.Format != docFormat {
		return nil, fmt.Errorf("not a quicksurvey survey document (format %q, want %q)",
			doc.Format, docFormat)
	}
	// A newer document may carry fields this build would drop, which is how a
	// "restore" quietly returns something other than what was saved.
	if doc.Version > docVersion {
		return nil, fmt.Errorf("document is version %d; this quicksurvey understands up to %d",
			doc.Version, docVersion)
	}
	title := strings.TrimSpace(doc.Survey.Title)
	if title == "" {
		return nil, fmt.Errorf("survey needs a title")
	}
	typ := doc.Survey.Type
	if typ == "" {
		typ = TypeThumbsUp
	}
	if typ != TypeThumbsUp {
		return nil, fmt.Errorf("unknown survey type %q", typ)
	}
	if len(doc.Responses) > 0 && !keepID {
		return nil, fmt.Errorf(
			"this backup carries %d responses, which can only be restored under the original "+
				"survey ID %q: a respondent's identifier is derived from it, so responses restored "+
				"under a new ID would never match anyone and the survey could be answered twice",
			len(doc.Responses), doc.SourceID)
	}
	if keepID && doc.SourceID == "" {
		return nil, fmt.Errorf("the document records no original survey ID to restore to")
	}
	if keepID {
		if _, exists := s.Survey(doc.SourceID); exists {
			return nil, fmt.Errorf("survey %q already exists; delete it first or restore without keeping the ID",
				doc.SourceID)
		}
	}

	now := time.Now().UTC()
	sv := &Survey{
		ID:           newID(surveyIDLen),
		Type:         typ,
		Title:        title,
		Description:  strings.TrimSpace(doc.Survey.Description),
		State:        StateDraft,
		ShowResults:  doc.Survey.ShowResults,
		AllowWriteIn: doc.Survey.AllowWriteIn,
		AllowComment: doc.Survey.AllowComment,
		NoRandomize:  doc.Survey.NoRandomize,
		Created:      now,
		Updated:      now,
	}
	if keepID {
		sv.ID = doc.SourceID
	}
	if doc.Full() {
		// Restored faithfully rather than normalised, so that a restore of a
		// published survey is still published and a restore of a draft is
		// still a draft.
		if doc.Survey.State != "" {
			if !validState(doc.Survey.State) {
				return nil, fmt.Errorf("unknown survey state %q", doc.Survey.State)
			}
			sv.State = doc.Survey.State
		}
		sv.CloseAt, sv.FirstOpenedAt = doc.Survey.CloseAt, doc.Survey.FirstOpenedAt
		if !doc.Survey.Created.IsZero() {
			sv.Created = doc.Survey.Created
		}
		if !doc.Survey.Updated.IsZero() {
			sv.Updated = doc.Survey.Updated
		}
	}

	// optionID maps what the document called an option to what it is called
	// once restored, so stored choices still point at the right thing.
	optionID := map[string]string{}
	for _, o := range doc.Survey.Options {
		text := strings.TrimSpace(o.Text)
		if text == "" {
			continue
		}
		source := o.Source
		if source != "writein" {
			source = "editor"
		}
		status := o.Status
		if status == "" {
			status = OptApproved
		}
		if !validOptionStatus(status) {
			return nil, fmt.Errorf("unknown option status %q", status)
		}
		created := o.Created
		if created.IsZero() {
			created = now
		}
		id := newID(optionIDLen)
		if keepID && o.ID != "" {
			id = o.ID
		}
		if o.ID != "" {
			optionID[o.ID] = id
		}
		sv.Options = append(sv.Options, Option{
			ID: id, Text: text, Status: status, Source: source, Created: created,
			MergedInto: o.MergedInto,
		})
	}
	// merged_into names another option, so it is remapped like any other
	// reference. This is a second pass because it may name an option that had
	// not been seen yet when the row was built.
	for i := range sv.Options {
		if m := sv.Options[i].MergedInto; m != "" {
			to, ok := optionID[m]
			if !ok {
				return nil, fmt.Errorf("option %q is merged into %q, which the document does not contain",
					sv.Options[i].ID, m)
			}
			sv.Options[i].MergedInto = to
		}
	}

	responses, err := restorableResponses(doc, optionID)
	if err != nil {
		return nil, err
	}

	if err := s.tx(func(tx *sql.Tx) error {
		if err := saveSurvey(tx, sv); err != nil {
			return err
		}
		return insertResponses(tx, sv.ID, responses)
	}); err != nil {
		return nil, err
	}
	return sv.Clone(), nil
}

// restorableResponses converts the document's responses, remapping option IDs
// and refusing any choice that names an option the document does not contain.
// A dangling choice would silently drop a vote.
func restorableResponses(doc *Document, optionID map[string]string) ([]*Response, error) {
	var out []*Response
	for i, dr := range doc.Responses {
		if dr.Voter == "" {
			return nil, fmt.Errorf("response %d has no voter identifier", i+1)
		}
		r := &Response{
			ID:      newID(responseIDLen),
			Voter:   dr.Voter,
			Comment: dr.Comment,
			Created: dr.Created,
			Updated: dr.Updated,
		}
		for _, c := range dr.Choices {
			to, ok := optionID[c]
			if !ok {
				return nil, fmt.Errorf("response %d chose option %q, which the document does not contain",
					i+1, c)
			}
			r.Choices = append(r.Choices, to)
		}
		for _, sn := range dr.Seen {
			to, ok := optionID[sn]
			if !ok {
				return nil, fmt.Errorf("response %d was shown option %q, which the document does not contain",
					i+1, sn)
			}
			r.Seen = append(r.Seen, to)
		}
		// A document written before seen existed, or one hand-edited, can
		// carry choices without them. Every chosen option was by definition on
		// that person's ballot, so seeding seen from choices is the weakest
		// claim that keeps Votes <= Shown true rather than restoring a survey
		// that reports fewer exposures than votes.
		if len(dr.Seen) == 0 {
			r.Seen = append([]string(nil), r.Choices...)
		}
		out = append(out, r)
	}
	return out, nil
}

func insertResponses(tx *sql.Tx, surveyID string, rs []*Response) error {
	for _, r := range rs {
		if _, err := tx.Exec(
			`INSERT INTO responses (id, survey_id, voter, comment, created, updated)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			r.ID, surveyID, r.Voter, r.Comment, dbTime(r.Created), dbTime(r.Updated)); err != nil {
			return err
		}
		for _, id := range r.Choices {
			if _, err := tx.Exec(
				`INSERT INTO choices (response_id, option_id) VALUES (?, ?)`, r.ID, id); err != nil {
				return err
			}
		}
		if err := insertSeen(tx, r.ID, r.Seen); err != nil {
			return err
		}
	}
	return nil
}

func validState(s string) bool {
	switch s {
	case StateDraft, StateOpen, StateClosed:
		return true
	}
	return false
}

func validOptionStatus(s string) bool {
	switch s {
	case OptApproved, OptPending, OptRejected, OptRemoved, OptMerged:
		return true
	}
	return false
}

// WriteSurveyDocument writes a survey out as indented JSON.
func (s *Store) WriteSurveyDocument(w io.Writer, id string, withResponses bool) error {
	doc, err := s.SurveyDocument(id, withResponses)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// ReadSurveyDocument restores a survey from JSON.
func (s *Store) ReadSurveyDocument(r io.Reader, keepID bool) (*Survey, error) {
	var doc Document
	dec := json.NewDecoder(r)
	// An unknown field means the file was written by something this build does
	// not understand. Restoring it anyway would drop the field in silence,
	// which is the failure mode a restore exists to rule out.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("reading survey document: %w", err)
	}
	return s.RestoreSurvey(&doc, keepID)
}
