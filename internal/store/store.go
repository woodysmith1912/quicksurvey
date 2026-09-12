// Package store implements QuickSurvey's persistence: a single SQLite database
// in a local directory.
//
//	<dir>/quicksurvey.db          everything — accounts, invitations, surveys,
//	                              options, responses, and the HMAC key
//
// SQLite rather than a hand-rolled file format because the deployment target is
// Kubernetes, where the platform will cheerfully start a second pod on your
// behalf. Two writers against plain files interleave and corrupt silently; two
// writers against SQLite serialise on POSIX locks and both stay correct. It is
// still a library rather than a service: one file, backed up by copying it, no
// daemon to run or patch.
//
// SQLite's locking is unreliable on NFS. On a block device — which is what a
// DigitalOcean Block Storage volume is — it behaves correctly.
package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure Go, so CGO_ENABLED=0 and a static image still work
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Store is the single point of access to persisted state. All exported methods
// are safe for concurrent use.
type Store struct {
	dir    string
	db     *sql.DB
	secret []byte

	// gen counts committed write transactions. Every write that can change a
	// tally goes through tx, so bumping it there makes cache invalidation
	// something the code cannot forget rather than something it must remember
	// at each of seven call sites.
	gen atomic.Uint64

	tallyMu    sync.Mutex
	tallyGen   uint64
	tallyCache map[string]cachedTally
}

// cachedTally is one survey's counted result, valid for a particular gen.
type cachedTally struct {
	results     []Result
	respondents int
}

const dbFile = "quicksurvey.db"

// MaxConns bounds the connection pool.
//
// One connection was the original choice, on the reasoning that writes
// serialise anyway. That reasoning was wrong: it serialised reads too, throwing
// away the concurrent readers WAL provides for free. BenchmarkBallot measured
// the cost at 2.2x — 611µs per ballot at one connection against 272µs at four.
//
// Four rather than more, because sixteen measured *worse* than four (328µs):
// past the point where readers overlap, the extra connections buy nothing and
// cost scheduling. Writes still serialise, which is correct and is what
// _txlock=immediate and busy_timeout handle. Both figures predate the seen
// set; the ratios they show are not expected to have changed.
var MaxConns = 4

// Open prepares dir as a data directory, creating it and the database if
// necessary.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, dbFile)

	// busy_timeout lets a writer wait for another rather than fail outright;
	// foreign_keys enforces the cascades the schema relies on; WAL keeps
	// readers from blocking the writer; synchronous=NORMAL is the usual WAL
	// trade — a crash can lose the last transaction, never the database.
	//
	// _txlock=immediate is the one that is easy to get wrong. A plain BEGIN is
	// deferred: the transaction starts as a reader and tries to become a
	// writer at its first write. If another connection wrote in between, that
	// upgrade fails with SQLITE_BUSY_SNAPSHOT, and busy_timeout cannot help,
	// because no amount of waiting makes the upgrade legal — the transaction
	// has to be thrown away and retried. Taking the write lock up front with
	// BEGIN IMMEDIATE turns that unrecoverable error into an ordinary wait.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Reads and writes share this pool. WAL lets many readers run at once
	// alongside one writer, so a pool of one throws away concurrency SQLite
	// offers for free — but writes still have to serialise, which is what
	// _txlock=immediate and busy_timeout above are for.
	//
	// MaxConns exists so this can be measured rather than argued about; see
	// BenchmarkBallot.
	db.SetMaxOpenConns(MaxConns)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	s := &Store{dir: dir, db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if s.secret, err = s.loadOrCreateSecret(); err != nil {
		db.Close()
		return nil, err
	}
	s.warnAboutLegacyFiles()
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Dir reports the data directory backing the store.
func (s *Store) Dir() string { return s.dir }

// schema is applied on every open. Every statement is idempotent, so this
// doubles as the migration for a fresh database and a no-op for an existing
// one.
const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
  name                 TEXT PRIMARY KEY,
  role                 TEXT NOT NULL,
  hash                 TEXT NOT NULL,
  created              TEXT NOT NULL,
  must_change_password INTEGER NOT NULL DEFAULT 0,
  pending              INTEGER NOT NULL DEFAULT 0,
  invited_by           TEXT NOT NULL DEFAULT '',
  sessions_from        TEXT
);

CREATE TABLE IF NOT EXISTS invites (
  id         TEXT PRIMARY KEY,
  digest     TEXT NOT NULL UNIQUE,
  role       TEXT NOT NULL,
  note       TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL,
  created    TEXT NOT NULL,
  expires    TEXT NOT NULL,
  claimed_by TEXT NOT NULL DEFAULT '',
  claimed    TEXT,
  revoked    INTEGER NOT NULL DEFAULT 0
);

-- Single-use password reset links, so an administrator never has to know
-- another person's password.
CREATE TABLE IF NOT EXISTS resets (
  id         TEXT PRIMARY KEY,
  digest     TEXT NOT NULL UNIQUE,
  user_name  TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created    TEXT NOT NULL,
  expires    TEXT NOT NULL,
  used       TEXT
);
CREATE INDEX IF NOT EXISTS resets_by_user ON resets(user_name);

CREATE TABLE IF NOT EXISTS surveys (
  id              TEXT PRIMARY KEY,
  type            TEXT NOT NULL,
  title           TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  state           TEXT NOT NULL,
  show_results    INTEGER NOT NULL DEFAULT 0,
  allow_write_in  INTEGER NOT NULL DEFAULT 1,
  allow_comment   INTEGER NOT NULL DEFAULT 1,
  no_randomize    INTEGER NOT NULL DEFAULT 0,
  close_at        TEXT,
  first_opened_at TEXT,
  created         TEXT NOT NULL,
  updated         TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS options (
  id          TEXT PRIMARY KEY,
  survey_id   TEXT NOT NULL REFERENCES surveys(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  text        TEXT NOT NULL,
  status      TEXT NOT NULL,
  source      TEXT NOT NULL,
  merged_into TEXT NOT NULL DEFAULT '',
  created     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS options_by_survey ON options(survey_id, position);

-- One row per respondent per survey. A second submission replaces the first,
-- which is what stops anyone inflating a count by resubmitting.
CREATE TABLE IF NOT EXISTS responses (
  id        TEXT PRIMARY KEY,
  survey_id TEXT NOT NULL REFERENCES surveys(id) ON DELETE CASCADE,
  voter     TEXT NOT NULL,
  comment   TEXT NOT NULL DEFAULT '',
  created   TEXT NOT NULL,
  updated   TEXT NOT NULL,
  UNIQUE(survey_id, voter)
);
CREATE INDEX IF NOT EXISTS responses_by_survey ON responses(survey_id, created);

CREATE TABLE IF NOT EXISTS choices (
  response_id TEXT NOT NULL REFERENCES responses(id) ON DELETE CASCADE,
  option_id   TEXT NOT NULL,
  PRIMARY KEY (response_id, option_id)
);

-- Which options were on the ballot a respondent submitted, unioned across all
-- of their submissions. Votes divided by this is the share of people who were
-- shown an option and picked it, which is the only fair figure for an option
-- added after some people had already answered.
CREATE TABLE IF NOT EXISTS seen (
  response_id TEXT NOT NULL REFERENCES responses(id) ON DELETE CASCADE,
  option_id   TEXT NOT NULL,
  PRIMARY KEY (response_id, option_id)
);
`

// addedColumns are columns introduced after a table first shipped.
//
// This list exists because CREATE TABLE IF NOT EXISTS does nothing at all to a
// table that already exists — including adding a column to it. A database
// created by an earlier version therefore keeps its old shape, every SELECT
// naming the new column fails, and the application reports no data rather than
// an error. That is exactly how a release shipped that could not read its own
// users table.
//
// Anything added to a table after its first release belongs here as well as in
// the schema above.
var addedColumns = []struct{ table, column, decl string }{
	{"users", "sessions_from", "TEXT"},
}

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
// set, once. Nothing recorded what was on those ballots, so for most statuses
// the assumption is the one the share-of-respondents figure already made:
// everyone who answered saw everything on the ballot. That holds for
// OptApproved (it was on the ballot), OptRemoved (it was on the ballot until
// an editor deleted it — everyone who answered still saw it, and narrowing
// this one would lose real exposures), and OptMerged (exposure resolves
// through the merge pointer to the target, which everyone saw anyway, and the
// tally dedupes per respondent so a duplicate exposure is harmless).
//
// OptPending and OptRejected are different: a pending write-in is visible
// only to the respondent who proposed it, and a rejected one was only ever
// visible to its proposer before a moderator refused it. Unlike the other
// statuses, the truth here is actually knowable — only the proposer could
// have a vote recorded for it — so instead of guessing "everyone," those two
// are backfilled as seen only by the response holding a vote for them. That
// also keeps the invariant Votes <= Shown intact: any response with a
// recorded vote for an option is guaranteed a seen row for it too, so no
// option can come out of the backfill with more votes than exposures.
// Guarded by a meta key rather than by the table being empty, so options
// added after the upgrade are never marked as shown to people who answered
// before they existed.
func (s *Store) backfillSeen() error {
	return s.tx(func(tx *sql.Tx) error {
		var done int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM meta WHERE key = 'seen_backfilled'`).Scan(&done); err != nil {
			return err
		}
		if done > 0 {
			return nil
		}
		// OptPending and OptRejected are excluded from the blanket "everyone
		// saw it" rule below (see the doc comment); the EXISTS clause instead
		// marks them seen only by whichever response actually voted for them.
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO seen (response_id, option_id)
			 SELECT r.id, o.id FROM responses r JOIN options o ON o.survey_id = r.survey_id
			 WHERE o.status NOT IN (?, ?)
			    OR EXISTS (SELECT 1 FROM choices c
			               WHERE c.response_id = r.id AND c.option_id = o.id)`,
			OptPending, OptRejected)
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

// ensureColumn adds a column to an existing table unless it is already there.
func (s *Store) ensureColumn(table, column, decl string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	slog.Info("upgrading the database", "table", table, "adding_column", column)
	// Identifiers cannot be bound as parameters, and these are compile-time
	// constants from addedColumns rather than anything a caller supplies.
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl)
	return err
}

// loadOrCreateSecret returns the instance's HMAC key, minting one on first use.
func (s *Store) loadOrCreateSecret() ([]byte, error) {
	var secret []byte
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'secret'`).Scan(&secret)
	if err == nil && len(secret) >= 32 {
		return secret, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	secret = make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('secret', ?)`, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// warnAboutLegacyFiles says something when a directory still holds the JSON
// files an earlier version wrote. They are not read, and not deleted either —
// silently ignoring data is worse than saying so.
func (s *Store) warnAboutLegacyFiles() {
	for _, name := range []string{"users.json", "invites.json", "secret.key", "surveys"} {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			slog.Warn("ignoring data from an earlier file-based version of quicksurvey",
				"path", filepath.Join(s.dir, name),
				"note", "it is not read and not deleted; move it aside once you no longer want it")
		}
	}
}

// MAC returns a keyed digest of parts, used wherever a value must be
// unforgeable or unlinkable: session cookies, CSRF tokens, voter identifiers,
// invitation links.
func (s *Store) MAC(parts ...string) string {
	h := hmac.New(sha256.New, s.secret)
	for _, p := range parts {
		// Length-prefix so ("ab","c") and ("a","bc") do not collide.
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// tx runs fn in a transaction, rolling back on error. This is what replaces the
// hand-written undo logic the file-based store needed.
func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Any committed write may have changed a tally: a response, a moderation
	// decision, a merge, a whole survey. Rather than reason about which, treat
	// every commit as invalidating. Writes are rare next to ballot loads on the
	// surveys where this matters, so the hit rate stays high, and there is no
	// way to add a write path that quietly serves stale counts.
	s.gen.Add(1)
	return nil
}

// --- time helpers ---------------------------------------------------------
//
// Times are stored as RFC3339 with nanoseconds, in UTC, so they sort lexically
// and survive a round trip. Optional times are NULL rather than a zero string.

func dbTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func dbTimePtr(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return dbTime(t)
}

func goTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func goTimePtr(s sql.NullString) time.Time {
	if !s.Valid {
		return time.Time{}
	}
	return goTime(s.String)
}

const idAlphabet = "abcdefghijkmnpqrstuvwxyz23456789" // no look-alikes

// newID returns a random, URL-safe, human-transcribable identifier.
func newID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is not a recoverable condition
	}
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

// InitialPasswordFile is where the bootstrap administrator's password is left
// on a brand-new instance.
//
// It goes in a file rather than the log because a log is the wrong place for a
// live credential: it is shipped to aggregators, retained long after the
// password is changed, and readable by anyone holding `kubectl logs` on the
// namespace — a strictly wider audience than the volume itself. The file is
// removed the moment that password is changed.
func (s *Store) InitialPasswordFile() string {
	return filepath.Join(s.dir, "initial-password")
}

// writeInitialPassword records the bootstrap credential for collection.
func (s *Store) writeInitialPassword(pw string) error {
	return writeFileAtomic(s.InitialPasswordFile(),
		[]byte(pw+"\n"), 0o600)
}

// clearInitialPassword removes it once it is no longer the way in.
func (s *Store) clearInitialPassword() {
	if err := os.Remove(s.InitialPasswordFile()); err != nil && !os.IsNotExist(err) {
		slog.Warn("could not remove the initial password file",
			"path", s.InitialPasswordFile(), "err", err)
	}
}

// writeFileAtomic replaces path in one step, so a reader never sees a
// half-written file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// BackupToWriter streams a consistent snapshot to w.
//
// It exists because the image is distroless: there is no tar in it, so
// `kubectl cp` cannot work, and no shell to redirect with. Streaming to stdout
// is the only way to get a file out of the container, and the binary has to
// provide it because nothing else in the image can.
//
// VACUUM INTO needs a real destination, so the snapshot is written beside the
// database and removed once it has been copied out.
func (s *Store) BackupToWriter(w io.Writer) error {
	tmp, err := os.CreateTemp(s.dir, ".backup-*")
	if err != nil {
		return err
	}
	path := tmp.Name()
	tmp.Close()
	// VACUUM INTO refuses to overwrite, so hand it a name that does not exist.
	if err := os.Remove(path); err != nil {
		return err
	}
	defer os.Remove(path)

	if _, err := s.db.Exec(`VACUUM INTO ` + quoteSQLString(path)); err != nil {
		return fmt.Errorf("preparing the snapshot: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// InitialPassword returns the bootstrap password if it has not yet been used.
//
// A subcommand rather than a documented `cat`: the image is distroless, so
// there is no cat to run and no shell to run it in.
func (s *Store) InitialPassword() (string, error) {
	b, err := os.ReadFile(s.InitialPasswordFile())
	if os.IsNotExist(err) {
		return "", fmt.Errorf("no initial password on file: it is removed once the " +
			"password has been changed, so this instance has already been set up")
	} else if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// BackupTo writes a consistent snapshot of the database to path, using SQLite's
// own VACUUM INTO. It is safe to run against a live database, which a plain
// file copy is not: WAL mode keeps recent commits in a side file, so copying
// quicksurvey.db alone can capture a torn or stale state.
//
// The result is an ordinary SQLite database. Restoring is copying it back.
func (s *Store) BackupTo(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite it", abs)
	} else if !os.IsNotExist(err) {
		return err
	}
	// VACUUM INTO takes its destination as a string literal, not a parameter.
	if _, err := s.db.Exec(`VACUUM INTO ` + quoteSQLString(abs)); err != nil {
		return fmt.Errorf("backing up to %s: %w", abs, err)
	}
	return nil
}

// quoteSQLString renders a Go string as a SQL string literal.
func quoteSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
