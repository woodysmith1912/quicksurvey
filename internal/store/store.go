// Package store implements QuickSurvey's persistence: plain files on a local
// disk, no database.
//
// Layout under the data directory:
//
//	secret.key                  HMAC key, generated on first start
//	users.json                  account records (password hashes, roles)
//	invites.json                outstanding invitation links (digests only)
//	surveys/<id>/survey.json    survey definition
//	surveys/<id>/responses.jsonl append-only response log
//
// Definitions are rewritten atomically (temp file + rename). Responses are only
// ever appended, so a crash can lose the last write but cannot corrupt earlier
// ones. Everything is also held in memory, which is what reads are served from;
// the files are the durable copy and are replayed at startup.
package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Store is the single point of access to on-disk state. All exported methods
// are safe for concurrent use.
type Store struct {
	dir    string
	secret []byte

	mu      sync.RWMutex
	users   map[string]*User
	invites map[string]*Invite
	surveys map[string]*Survey
	// latest response per voter, per survey. A voter who answers twice
	// replaces their earlier answer, so counts cannot be inflated.
	responses map[string]map[string]*Response
}

// Open prepares dir as a data directory, creating it if necessary, and loads
// existing state into memory.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "surveys"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		dir:       dir,
		users:     map[string]*User{},
		invites:   map[string]*Invite{},
		surveys:   map[string]*Survey{},
		responses: map[string]map[string]*Response{},
	}
	var err error
	if s.secret, err = loadOrCreateSecret(filepath.Join(dir, "secret.key")); err != nil {
		return nil, err
	}
	if err := s.loadUsers(); err != nil {
		return nil, err
	}
	if err := s.loadInvites(); err != nil {
		return nil, err
	}
	if err := s.loadSurveys(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir reports the data directory backing the store.
func (s *Store) Dir() string { return s.dir }

// MAC returns a keyed digest of parts, used wherever a value must be
// unforgeable or unlinkable: session cookies, CSRF tokens, voter identifiers.
func (s *Store) MAC(parts ...string) string {
	h := hmac.New(sha256.New, s.secret)
	for _, p := range parts {
		// Length-prefix so ("ab","c") and ("a","bc") do not collide.
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func loadOrCreateSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil && len(b) >= 32:
		return b, nil
	case err != nil && !os.IsNotExist(err):
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, secret, 0o600); err != nil {
		return nil, err
	}
	return secret, nil
}

// writeFileAtomic replaces path in one step, so a reader (or a crash) never
// observes a half-written file.
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

func writeJSONAtomic(path string, v any, perm os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), perm)
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
