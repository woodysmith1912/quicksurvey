package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

func (s *Store) invitesPath() string { return filepath.Join(s.dir, "invites.json") }

func (s *Store) loadInvites() error {
	b, err := os.ReadFile(s.invitesPath())
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	var list []*Invite
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("invites.json: %w", err)
	}
	for _, iv := range list {
		s.invites[iv.ID] = iv
	}
	return nil
}

// saveInvites must be called with s.mu held.
func (s *Store) saveInvites() error {
	list := make([]*Invite, 0, len(s.invites))
	for _, iv := range s.invites {
		list = append(list, iv)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.After(list[j].Created) })
	return writeJSONAtomic(s.invitesPath(), list, 0o600)
}

// InviteTTL bounds how long an unclaimed link stays usable.
const InviteTTL = 7 * 24 * time.Hour

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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invites[iv.ID] = iv
	if err := s.saveInvites(); err != nil {
		delete(s.invites, iv.ID)
		return nil, "", err
	}
	return iv, token, nil
}

// InviteByToken resolves the secret from a URL to an invite that can still be
// claimed. The comparison is constant-time against every stored digest, so a
// caller cannot learn which invites exist by timing the lookup.
func (s *Store) InviteByToken(token string) (*Invite, bool) {
	if token == "" {
		return nil, false
	}
	want := s.MAC("invite", token)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found *Invite
	for _, iv := range s.invites {
		if subtle.ConstantTimeCompare([]byte(iv.Digest), []byte(want)) == 1 {
			found = iv
		}
	}
	if found == nil || !found.Open(time.Now()) {
		return nil, false
	}
	c := *found
	return &c, true
}

// Invites returns every invite, newest first.
func (s *Store) Invites() []*Invite {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Invite, 0, len(s.invites))
	for _, iv := range s.invites {
		c := *iv
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// RevokeInvite closes an unclaimed link.
func (s *Store) RevokeInvite(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	iv, ok := s.invites[id]
	if !ok {
		return fmt.Errorf("invite %q: %w", id, ErrNotFound)
	}
	if iv.ClaimedBy != "" {
		return fmt.Errorf("that invite has already been claimed")
	}
	iv.Revoked = true
	return s.saveInvites()
}

// ClaimInvite creates a pending account from an invite, in one step so a single
// link cannot produce two accounts. The account exists but can do nothing until
// an administrator approves it.
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

	s.mu.Lock()
	defer s.mu.Unlock()

	var iv *Invite
	for _, cand := range s.invites {
		if subtle.ConstantTimeCompare([]byte(cand.Digest), []byte(want)) == 1 {
			iv = cand
		}
	}
	if iv == nil || !iv.Open(time.Now()) {
		return nil, fmt.Errorf("that invitation link is no longer valid")
	}
	if _, taken := s.users[name]; taken {
		return nil, fmt.Errorf("the username %q is already taken", name)
	}

	now := time.Now().UTC()
	u := &User{
		Name:      name,
		Role:      iv.Role,
		Hash:      hash,
		Created:   now,
		Pending:   true,
		InvitedBy: iv.CreatedBy,
	}
	s.users[name] = u
	iv.ClaimedBy, iv.Claimed = name, now

	if err := s.saveUsers(); err != nil {
		delete(s.users, name)
		iv.ClaimedBy, iv.Claimed = "", time.Time{}
		return nil, err
	}
	if err := s.saveInvites(); err != nil {
		// The account exists and the link is spent in memory; leaving the file
		// stale would let the link be reused after a restart, so undo instead.
		delete(s.users, name)
		iv.ClaimedBy, iv.Claimed = "", time.Time{}
		_ = s.saveUsers()
		return nil, err
	}
	return u, nil
}

// ApproveUser clears the pending flag and sets the account's role.
func (s *Store) ApproveUser(name string, role Role) error {
	if !ValidRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return fmt.Errorf("user %q: %w", name, ErrNotFound)
	}
	if !u.Pending {
		return fmt.Errorf("%q is already approved", name)
	}
	prevRole, prevPending := u.Role, u.Pending
	u.Role, u.Pending = role, false
	if err := s.saveUsers(); err != nil {
		u.Role, u.Pending = prevRole, prevPending
		return err
	}
	return nil
}

// PendingUsers returns accounts awaiting approval, oldest first.
func (s *Store) PendingUsers() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*User
	for _, u := range s.users {
		if u.Pending {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}
