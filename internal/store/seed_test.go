package store

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSeedLargeDataset builds a realistically sized database and reports how
// long it takes. Skipped unless QS_SEED_DIR is set, because it is a load
// fixture rather than a test of behaviour.
//
//	QS_SEED_DIR=/var/tmp/qs-seed go test ./internal/store -run SeedLargeDataset -v -timeout 60m
//
// Everything goes through the ordinary store API — one transaction per survey
// and one per response, exactly as the running application does. Bulk-inserting
// would produce a faster number that answers a question nobody asked.
func TestSeedLargeDataset(t *testing.T) {
	dir := os.Getenv("QS_SEED_DIR")
	if dir == "" {
		t.Skip("set QS_SEED_DIR to build the load-test fixture")
	}
	surveys := envInt(t, "QS_SEED_SURVEYS", 10_000)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rng := rand.New(rand.NewPCG(1, 2)) // deterministic, so runs compare
	start := time.Now()
	var options, responses, choices int
	ids := make([]string, 0, surveys)

	for i := range surveys {
		nOpts := 5 + rng.IntN(11) // 5..15
		opts := make([]string, nOpts)
		for j := range opts {
			opts[j] = fmt.Sprintf("Survey %d option %d", i, j)
		}
		sv, err := s.CreateSurvey(fmt.Sprintf("Load test survey %d", i), "seeded", opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Publish(sv.ID); err != nil {
			t.Fatal(err)
		}
		options += nOpts
		ids = append(ids, sv.ID)

		nResp := 5 + rng.IntN(6) // 5..10
		for k := range nResp {
			// Each respondent thumbs-up a random subset, as a real one would.
			var picked []string
			for _, o := range sv.Options {
				if rng.IntN(3) == 0 {
					picked = append(picked, o.ID)
				}
			}
			if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, fmt.Sprintf("v%d-%d", i, k)),
				picked, ""); err != nil {
				t.Fatal(err)
			}
			responses++
			choices += len(picked)
		}

		if (i+1)%1000 == 0 {
			t.Logf("%6d surveys  %8d responses  %s elapsed", i+1, responses,
				time.Since(start).Round(time.Millisecond))
		}
	}
	elapsed := time.Since(start)

	// Leave a manifest so the benchmark can pick surveys out of this fixture.
	manifest := filepath.Join(dir, "seeded-survey-ids.txt")
	f, err := os.Create(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		fmt.Fprintln(f, id)
	}
	f.Close()

	fi, _ := os.Stat(filepath.Join(dir, dbFile))
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	t.Logf("")
	t.Logf("seeded %d surveys, %d options, %d responses, %d choices",
		surveys, options, responses, choices)
	t.Logf("elapsed %s  (%.0f surveys/s, %.0f writes/s)",
		elapsed.Round(time.Millisecond),
		float64(surveys)/elapsed.Seconds(),
		float64(surveys+responses)/elapsed.Seconds())
	t.Logf("database %.1f MB at %s", float64(size)/(1<<20), dir)
	t.Logf("manifest %s", manifest)
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		t.Fatalf("%s=%q is not a positive integer", key, v)
	}
	return n
}
