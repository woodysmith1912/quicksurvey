// Package export renders survey results as tab-separated values.
//
// TSV rather than CSV because Google Sheets imports it without a dialect
// dialog, and because the only free-text fields (option labels and comments)
// commonly contain commas but never contain tabs once sanitised.
package export

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

// cell makes a value safe to place between tabs. Tabs and newlines inside free
// text would otherwise shift every following column.
func cell(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// textCell is cell for a value someone else wrote — an option's text or a
// respondent's comment.
//
// Spreadsheets treat a cell beginning with =, +, - or @ as a formula, so text
// this file carries verbatim becomes executable on import: =HYPERLINK and
// =IMPORTDATA can exfiltrate the rest of the sheet, and Excel additionally
// honours =cmd| for DDE. Since the entire point of this export is that you drop
// it straight into Google Sheets, a comment must not be able to run there.
//
// The leading apostrophe is OWASP's recommendation. Sheets treats it as the
// "this is text" marker and does not display it.
func textCell(s string) string {
	s = cell(s)
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@':
		return "'" + s
	}
	return s
}

func row(w io.Writer, fields ...string) error {
	for i, f := range fields {
		if i > 0 {
			if _, err := io.WriteString(w, "\t"); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, cell(f)); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// header labels a column, marking options that are no longer live so a reader
// can tell an unselected option from a withdrawn one.
func header(o store.Option) string {
	if o.Status == store.OptApproved {
		return textCell(o.Text)
	}
	return textCell(fmt.Sprintf("%s [%s]", o.Text, o.Status))
}

// Responses writes the wide, one-row-per-response table: a column per option
// holding 1 (picked), 0 (shown, not picked) or blank (never on that person's ballot), plus the comment.
// This is the shape you pivot in Sheets.
//
// Merged options are omitted as columns because their votes already appear
// under the option they were merged into.
func Responses(w io.Writer, sv *store.Survey, responses []*store.Response, loc *time.Location) error {
	bw := bufio.NewWriter(w)

	var cols []store.Option
	for _, o := range sv.Options {
		if o.Status != store.OptMerged {
			cols = append(cols, o)
		}
	}

	head := []string{"response_id", "submitted", "updated"}
	for _, o := range cols {
		head = append(head, header(o))
	}
	head = append(head, "selections", "comment")
	if err := row(bw, head...); err != nil {
		return err
	}

	for _, r := range responses {
		rec := []string{r.ID, r.Created.In(loc).Format(time.RFC3339), r.Updated.In(loc).Format(time.RFC3339)}
		n := 0
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
		rec = append(rec, fmt.Sprint(n), textCell(r.Comment))
		if err := row(bw, rec...); err != nil {
			return err
		}
	}
	return bw.Flush()
}

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

// Filename returns a download name that sorts sensibly and survives a file
// manager: no spaces, no punctuation beyond dash and dot.
func Filename(sv *store.Survey, kind string, now time.Time) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		case r == ' ', r == '-', r == '_':
			return '-'
		}
		return -1
	}, sv.Title)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = sv.ID
	}
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	return fmt.Sprintf("%s-%s-%s.tsv", slug, kind, now.Format("20060102"))
}
