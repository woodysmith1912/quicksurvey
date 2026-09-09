package store

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// Reset is a single-use link that lets someone choose a new password for their
// own account.
//
// It exists so that an administrator never has to know, type, or transmit
// another person's password. An admin who can set a password can sign in as
// that person; an admin who can only hand out a reset link cannot. The account
// owner is the only one who ever sees the new secret.
//
// As with invitations, the secret in the URL is never stored — only a keyed
// digest of it — so a leaked database yields no working links.
type Reset struct {
	ID        string    `json:"id"`
	Digest    string    `json:"digest"`
	User      string    `json:"user"`
	CreatedBy string    `json:"created_by"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
	Used      time.Time `json:"used,omitzero"`
}

// Open reports whether the link can still be used.
func (rp *Reset) Open(now time.Time) bool {
	return rp.Used.IsZero() && now.Before(rp.Expires)
}

// ResetTTL is deliberately shorter than an invitation's. An invitation waits
// for someone to get round to it; a password reset is handed over during a
// conversation and used immediately.
const ResetTTL = 2 * time.Hour

const resetColumns = `id, digest, user_name, created_by, created, expires, used`

func scanReset(row rowScanner) (*Reset, error) {
	var rp Reset
	var created, expires string
	var used sql.NullString
	if err := row.Scan(&rp.ID, &rp.Digest, &rp.User, &rp.CreatedBy, &created, &expires, &used); err != nil {
		return nil, err
	}
	rp.Created, rp.Expires, rp.Used = goTime(created), goTime(expires), goTimePtr(used)
	return &rp, nil
}

// CreateReset mints a reset link for an account and returns the secret to put
// in the URL, once. Any earlier unused link for that account is retired, so a
// second request invalidates the first rather than leaving two ways in.
func (s *Store) CreateReset(user, createdBy string) (*Reset, string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)

	now := time.Now().UTC()
	rp := &Reset{
		ID:        newID(8),
		Digest:    s.MAC("reset", token),
		User:      user,
		CreatedBy: createdBy,
		Created:   now,
		Expires:   now.Add(ResetTTL),
	}
	err := s.tx(func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE name = ?`, user).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("user %q: %w", user, ErrNotFound)
		}
		// Retire anything outstanding for this account.
		if _, err := tx.Exec(
			`UPDATE resets SET used = ? WHERE user_name = ? AND used IS NULL`,
			dbTime(now), user); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO resets (`+resetColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rp.ID, rp.Digest, rp.User, rp.CreatedBy, dbTime(rp.Created), dbTime(rp.Expires), nil)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return rp, token, nil
}

// ResetByToken resolves a URL secret to a link that can still be used.
func (s *Store) ResetByToken(token string) (*Reset, bool) {
	if token == "" {
		return nil, false
	}
	rp, err := findResetByToken(s.db, s.MAC("reset", token))
	if err != nil || !rp.Open(time.Now()) {
		return nil, false
	}
	return rp, true
}

// findResetByToken compares in constant time against every stored digest, so
// lookup time does not reveal which links exist.
func findResetByToken(q queryer, want string) (*Reset, error) {
	rows, err := q.Query(`SELECT ` + resetColumns + ` FROM resets WHERE used IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found *Reset
	for rows.Next() {
		rp, err := scanReset(rows)
		if err != nil {
			return nil, err
		}
		if subtle.ConstantTimeCompare([]byte(rp.Digest), []byte(want)) == 1 {
			found = rp
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// UseReset sets a new password and spends the link, in one transaction so a
// single link cannot set two passwords. Existing sessions for the account are
// revoked: a password reset is usually a response to something going wrong, and
// leaving old cookies alive would defeat the point.
func (s *Store) UseReset(token, password string) (string, error) {
	if len(password) < 8 {
		return "", fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return "", err
	}
	want := s.MAC("reset", token)

	var user string
	err = s.tx(func(tx *sql.Tx) error {
		rp, err := findResetByToken(tx, want)
		if err != nil || !rp.Open(time.Now()) {
			return fmt.Errorf("that reset link is no longer valid")
		}
		now := time.Now().UTC()
		res, err := tx.Exec(
			`UPDATE users SET hash = ?, must_change_password = 0, sessions_from = ? WHERE name = ?`,
			hash, dbTime(now), rp.User)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("user %q: %w", rp.User, ErrNotFound)
		}
		if _, err := tx.Exec(`UPDATE resets SET used = ? WHERE id = ?`, dbTime(now), rp.ID); err != nil {
			return err
		}
		user = rp.User
		return nil
	})
	if err != nil {
		return "", err
	}
	s.clearInitialPassword()
	return user, nil
}

// OutstandingReset returns the unused link for an account, if any, so the
// accounts page can say one is already out there.
func (s *Store) OutstandingReset(user string) (*Reset, bool) {
	rp, err := scanReset(s.db.QueryRow(
		`SELECT `+resetColumns+` FROM resets WHERE user_name = ? AND used IS NULL`, user))
	if err != nil || errors.Is(err, sql.ErrNoRows) || !rp.Open(time.Now()) {
		return nil, false
	}
	return rp, true
}
