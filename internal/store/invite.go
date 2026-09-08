package store

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Invite is a single-use link that lets someone create an account without an
// existing administrator typing their password.
//
// The secret in the URL is never stored. Only a keyed digest of it is kept, so
// a leaked invites.json yields no working links — the same reasoning as the
// voter identifiers.
type Invite struct {
	ID        string    `json:"id"`
	Digest    string    `json:"digest"` // MAC of the token in the URL
	Role      Role      `json:"role"`   // role granted when an admin approves
	Note      string    `json:"note,omitempty"`
	CreatedBy string    `json:"created_by"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
	ClaimedBy string    `json:"claimed_by,omitempty"`
	Claimed   time.Time `json:"claimed,omitzero"`
	Revoked   bool      `json:"revoked,omitempty"`
}

// Open reports whether the invite can still be claimed.
func (i *Invite) Open(now time.Time) bool {
	return !i.Revoked && i.ClaimedBy == "" && now.Before(i.Expires)
}

// Status is a human-readable state for the accounts page.
func (i *Invite) Status(now time.Time) string {
	switch {
	case i.Revoked:
		return "revoked"
	case i.ClaimedBy != "":
		return "claimed by " + i.ClaimedBy
	case !now.Before(i.Expires):
		return "expired"
	default:
		return "open"
	}
}

// InviteTTL bounds how long an unclaimed link stays usable.
const InviteTTL = 7 * 24 * time.Hour

const inviteColumns = `id, digest, role, note, created_by, created, expires, claimed_by, claimed, revoked`

func scanInvite(row rowScanner) (*Invite, error) {
	var iv Invite
	var created, expires string
	var claimed sql.NullString
	if err := row.Scan(&iv.ID, &iv.Digest, &iv.Role, &iv.Note, &iv.CreatedBy,
		&created, &expires, &iv.ClaimedBy, &claimed, &iv.Revoked); err != nil {
		return nil, err
	}
	iv.Created, iv.Expires, iv.Claimed = goTime(created), goTime(expires), goTimePtr(claimed)
	return &iv, nil
}

// CreateInvite mints an invite and returns it together with the secret to put
// in the URL. The secret is returned exactly once and cannot be recovered.
func (s *Store) CreateInvite(createdBy string, role Role, note string, ttl time.Duration) (*Invite, string, error) {
	if !ValidRole(role) {
		return nil, "", fmt.Errorf("unknown role %q", role)
	}
	if ttl <= 0 {
		ttl = InviteTTL
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)

	now := time.Now().UTC()
	iv := &Invite{
		ID:        newID(8),
		Digest:    s.MAC("invite", token),
		Role:      role,
		Note:      strings.TrimSpace(note),
		CreatedBy: createdBy,
		Created:   now,
		Expires:   now.Add(ttl),
	}
	if _, err := s.db.Exec(
		`INSERT INTO invites (`+inviteColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		iv.ID, iv.Digest, iv.Role, iv.Note, iv.CreatedBy,
		dbTime(iv.Created), dbTime(iv.Expires), "", nil, false); err != nil {
		return nil, "", err
	}
	return iv, token, nil
}

// InviteByToken resolves the secret from a URL to an invite that can still be
// claimed. The digest comparison is constant-time, so a caller cannot learn
// which invitations exist by timing the lookup.
func (s *Store) InviteByToken(token string) (*Invite, bool) {
	if token == "" {
		return nil, false
	}
	iv, err := findInviteByToken(s.db, s.MAC("invite", token))
	if err != nil || !iv.Open(time.Now()) {
		return nil, false
	}
	return iv, true
}

// findInviteByToken scans every invite and compares in constant time, rather
// than letting SQLite match the digest, so lookup time does not depend on which
// invitations exist.
func findInviteByToken(q queryer, want string) (*Invite, error) {
	rows, err := q.Query(`SELECT ` + inviteColumns + ` FROM invites`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found *Invite
	for rows.Next() {
		iv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		if subtle.ConstantTimeCompare([]byte(iv.Digest), []byte(want)) == 1 {
			found = iv
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

// Invites returns every invite, newest first.
func (s *Store) Invites() []*Invite {
	rows, err := s.db.Query(`SELECT ` + inviteColumns + ` FROM invites ORDER BY created DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Invite
	for rows.Next() {
		iv, err := scanInvite(rows)
		if err != nil {
			return out
		}
		out = append(out, iv)
	}
	return out
}

// RevokeInvite closes an unclaimed link.
func (s *Store) RevokeInvite(id string) error {
	return s.tx(func(tx *sql.Tx) error {
		var claimedBy string
		err := tx.QueryRow(`SELECT claimed_by FROM invites WHERE id = ?`, id).Scan(&claimedBy)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("invite %q: %w", id, ErrNotFound)
		} else if err != nil {
			return err
		}
		if claimedBy != "" {
			return fmt.Errorf("that invite has already been claimed")
		}
		_, err = tx.Exec(`UPDATE invites SET revoked = 1 WHERE id = ?`, id)
		return err
	})
}

// ClaimInvite creates a pending account from an invite. Creating the account
// and spending the link happen in one transaction, which is what makes the link
// genuinely single-use rather than single-use-if-nobody-races-you. The account
// exists but can do nothing until an administrator approves it.
func (s *Store) ClaimInvite(token, name, password string) (*User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("choose a username")
	}
	if len(password) < 8 {
		return nil, fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	want := s.MAC("invite", token)

	var u *User
	err = s.tx(func(tx *sql.Tx) error {
		iv, err := findInviteByToken(tx, want)
		if err != nil || !iv.Open(time.Now()) {
			return fmt.Errorf("that invitation link is no longer valid")
		}
		var taken int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE name = ?`, name).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			return fmt.Errorf("the username %q is already taken", name)
		}
		now := time.Now().UTC()
		u = &User{
			Name: name, Role: iv.Role, Hash: hash, Created: now,
			Pending: true, InvitedBy: iv.CreatedBy,
		}
		if _, err := tx.Exec(
			`INSERT INTO users (`+userColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			u.Name, u.Role, u.Hash, dbTime(u.Created), false, true, u.InvitedBy); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE invites SET claimed_by = ?, claimed = ? WHERE id = ?`,
			name, dbTime(now), iv.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}
