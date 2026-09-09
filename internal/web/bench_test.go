package web

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

// Benchmarks for the two questions worth asking about this application:
// how fast can it serve a ballot to someone who has not answered, and to
// someone who has. Everything here goes through real HTTP and real SQLite —
// the same driver, DSN, WAL settings and connection pool as production. Only
// the proxy and the pod's CPU limit are absent.
//
// Run:
//
//	go test ./internal/web -bench Ballot -benchmem -run '^$'
//	go test ./internal/web -bench Ballot -benchtime 5s -cpuprofile cpu.out -run '^$'
//
// QS_BENCH_FIXTURE points at a directory seeded by TestSeedLargeDataset, so the
// benchmark runs against a realistically sized database — 10,000 surveys rather
// than the one it would otherwise build. The survey it reads is picked from the
// middle of that fixture, so index lookups are not flattered by locality.
//
// QS_BENCH_DIR points the database at real disk. Without it the data lands
// wherever t.TempDir() does, which may be tmpfs, where fsync is nearly free —
// harmless for these read benchmarks, misleading for anything that writes.

const (
	benchOptions  = 30 // a realistically large survey
	benchSelected = 10 // "roughly 10 of the options", per the question
)

// benchEnv is one running server with a populated survey.
type benchEnv struct {
	srv     *httptest.Server
	surveyw *store.Survey
	st      *store.Store
}

// existingFixture opens a seeded database, if one was pointed at, and returns a
// survey from the middle of it.
func existingFixture(b *testing.B, showResults bool) (*benchEnv, bool) {
	b.Helper()
	dir := os.Getenv("QS_BENCH_FIXTURE")
	if dir == "" {
		return nil, false
	}
	st, err := store.Open(dir)
	if err != nil {
		b.Fatalf("opening the fixture at %s: %v", dir, err)
	}
	b.Cleanup(func() { st.Close() })

	ids, err := os.ReadFile(filepath.Join(dir, "seeded-survey-ids.txt"))
	if err != nil {
		b.Fatalf("no manifest in the fixture: %v", err)
	}
	lines := strings.Fields(string(ids))
	if len(lines) == 0 {
		b.Fatal("the fixture manifest is empty")
	}
	sv, ok := st.Survey(lines[len(lines)/2]) // the middle, not the first
	if !ok {
		b.Fatal("the survey named in the manifest is not in the database")
	}
	if showResults {
		if sv, err = st.UpdateSurvey(sv.ID, func(d *store.Survey) error {
			d.ShowResults = true
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	s, err := New(st, Config{
		Location: time.UTC, Logger: slog.New(slog.DiscardHandler),
		LoginRate: -1, VoterRate: -1,
	})
	if err != nil {
		b.Fatal(err)
	}
	srv := httptest.NewServer(s)
	b.Cleanup(srv.Close)
	return &benchEnv{srv: srv, surveyw: sv, st: st}, true
}

func newBenchEnv(b *testing.B, respondents int, showResults bool) *benchEnv {
	b.Helper()
	if e, ok := existingFixture(b, showResults); ok {
		return e
	}
	dir := os.Getenv("QS_BENCH_DIR")
	if dir == "" {
		dir = b.TempDir()
	} else {
		dir = mkBenchDir(b, dir)
	}
	st, err := store.Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })

	opts := make([]string, benchOptions)
	for i := range opts {
		opts[i] = fmt.Sprintf("Option number %d", i)
	}
	sv, err := st.CreateSurvey("Benchmark survey", "how fast is this", opts)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := st.Publish(sv.ID); err != nil {
		b.Fatal(err)
	}
	if showResults {
		if sv, err = st.UpdateSurvey(sv.ID, func(d *store.Survey) error {
			d.ShowResults = true
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	// Background population, so Tally has something to walk.
	for i := range respondents {
		voter := st.VoterID(sv.ID, fmt.Sprintf("background-%d", i))
		var choices []string
		for j := range benchSelected {
			choices = append(choices, sv.Options[(i+j)%benchOptions].ID)
		}
		if _, err := st.SaveResponse(sv.ID, voter, choices, ""); err != nil {
			b.Fatal(err)
		}
	}

	s, err := New(st, Config{
		Location: time.UTC,
		Logger:   slog.New(slog.DiscardHandler),
		// The limiter is not what is being measured, and a benchmark from one
		// address looks exactly like an attack.
		LoginRate: -1, VoterRate: -1,
	})
	if err != nil {
		b.Fatal(err)
	}
	srv := httptest.NewServer(s)
	b.Cleanup(srv.Close)
	return &benchEnv{srv: srv, surveyw: sv, st: st}
}

func mkBenchDir(b *testing.B, base string) string {
	b.Helper()
	dir, err := os.MkdirTemp(base, "qs-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

var benchCSRF = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// benchClient is one browser, with its own cookie jar so it holds a voter
// identity across requests exactly as a real respondent does.
func benchClient(b *testing.B, base string) *http.Client {
	b.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		b.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// get fetches a URL and discards the body, which must still be read in full or
// the connection is not reusable and the benchmark measures dialling.
func get(b *testing.B, c *http.Client, url string) {
	resp, err := c.Get(url)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		b.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("status %d", resp.StatusCode)
	}
}

// vote makes a client into a respondent who has selected n options, so that
// their subsequent ballot loads exercise the response and choices lookups.
func (e *benchEnv) vote(b *testing.B, c *http.Client, n int) {
	b.Helper()
	path := e.srv.URL + "/s/" + e.surveyw.ID
	resp, err := c.Get(path)
	if err != nil {
		b.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := benchCSRF.FindSubmatch(body)
	if m == nil {
		b.Fatal("no CSRF token on the ballot")
	}
	// A seeded survey has between 5 and 15 options, so "select ten" has to mean
	// "select ten, or all of them if there are fewer".
	if n > len(e.surveyw.Options) {
		n = len(e.surveyw.Options)
	}
	form := url.Values{"csrf": {string(m[1])}}
	for i := range n {
		form.Add("choice", e.surveyw.Options[i].ID)
	}
	r, err := c.PostForm(path+"/vote", form)
	if err != nil {
		b.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
}

// BenchmarkBallot answers both questions, across pool sizes, so "is one
// connection the bottleneck" comes out as a number rather than an opinion.
func BenchmarkBallot(b *testing.B) {
	for _, conns := range []int{1, 4, 16} {
		for _, c := range []struct {
			name        string
			selected    int
			respondents int
			showResults bool
		}{
			{"fresh", 0, 0, false},
			{"selected10", benchSelected, 0, false},
			{"selected10_1kOthers", benchSelected, 1000, false},
			{"selected10_results_1kOthers", benchSelected, 1000, true},
		} {
			b.Run(fmt.Sprintf("conns=%d/%s", conns, c.name), func(b *testing.B) {
				old := store.MaxConns
				store.MaxConns = conns
				defer func() { store.MaxConns = old }()

				e := newBenchEnv(b, c.respondents, c.showResults)
				path := e.srv.URL + "/s/" + e.surveyw.ID

				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					cl := benchClient(b, e.srv.URL)
					if c.selected > 0 {
						b.StopTimer()
						e.vote(b, cl, c.selected)
						b.StartTimer()
					} else {
						// Take a voter cookie first, so the measurement is of
						// serving a ballot rather than of minting an identity.
						b.StopTimer()
						get(b, cl, path)
						b.StartTimer()
					}
					for pb.Next() {
						get(b, cl, path)
					}
				})
			})
		}
	}
}
