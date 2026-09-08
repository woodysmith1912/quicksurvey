package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Role determines what an account may do. Roles are ordered: an admin can do
// anything an editor can, an editor anything a viewer can.
type Role string

const (
	RoleViewer Role = "viewer" // read results
	RoleEditor Role = "editor" // + create and edit surveys
	RoleAdmin  Role = "admin"  // + manage accounts
)

var roleRank = map[Role]int{RoleViewer: 1, RoleEditor: 2, RoleAdmin: 3}

// ValidRole reports whether r is one of the three known roles.
func ValidRole(r Role) bool { _, ok := roleRank[r]; return ok }

// AtLeast reports whether r carries the privileges of want.
func (r Role) AtLeast(want Role) bool { return roleRank[r] >= roleRank[want] }

// User is an authenticated account. Accounts exist only to administer surveys;
// respondents never have one.
type User struct {
	Name    string    `json:"name"`
	Role    Role      `json:"role"`
	Hash    string    `json:"hash"` // PHC-ish string, see hashPassword
	Created time.Time `json:"created"`
	// MustChangePassword is set on the bootstrap account so the random
	// password printed to the container log cannot become permanent.
	MustChangePassword bool `json:"must_change_password,omitempty"`
	// Pending marks an account created from an invitation that no
	// administrator has approved yet. It can sign in and do nothing else.
	Pending   bool   `json:"pending,omitempty"`
	InvitedBy string `json:"invited_by,omitempty"`
}

const pbkdf2Iterations = 600_000 // OWASP guidance for PBKDF2-HMAC-SHA256

// hashPassword returns an encoded verifier of the form
// pbkdf2-sha256$<iter>$<salt>$<key>, self-describing so the cost can be raised
// later without invalidating existing accounts.
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Store) usersPath() string { return filepath.Join(s.dir, "users.json") }

func (s *Store) loadUsers() error {
	b, err := os.ReadFile(s.usersPath())
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	var list []*User
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("users.json: %w", err)
	}
	for _, u := range list {
		s.users[u.Name] = u
	}
	return nil
}

// saveUsers must be called with s.mu held.
func (s *Store) saveUsers() error {
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return writeJSONAtomic(s.usersPath(), list, 0o600)
}

// AddUser creates an account. It fails if the name is taken.
func (s *Store) AddUser(name string, role Role, password string) (*User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("username must not be empty")
	}
	if !ValidRole(role) {
		return nil, fmt.Errorf("unknown role %q", role)
	}
	if len(password) < 8 {
		return nil, fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[name]; ok {
		return nil, fmt.Errorf("user %q: %w", name, ErrExists)
	}
	u := &User{Name: name, Role: role, Hash: hash, Created: time.Now().UTC()}
	s.users[name] = u
	if err := s.saveUsers(); err != nil {
		delete(s.users, name)
		return nil, err
	}
	return u, nil
}

// SetPassword replaces a user's password, which also invalidates their existing
// sessions (session MACs are bound to the password hash).
func (s *Store) SetPassword(name, password string) error {
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return fmt.Errorf("user %q: %w", name, ErrNotFound)
	}
	prev, prevMust := u.Hash, u.MustChangePassword
	u.Hash, u.MustChangePassword = hash, false
	if err := s.saveUsers(); err != nil {
		u.Hash, u.MustChangePassword = prev, prevMust
		return err
	}
	return nil
}

// AddUserMustChange creates an account that is required to set a new password
// before it can do anything else. Used for the bootstrap admin.
func (s *Store) AddUserMustChange(name string, role Role, password string) (*User, error) {
	u, err := s.AddUser(name, role, password)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u.MustChangePassword = true
	return u, s.saveUsers()
}

// SetRole changes a user's role.
func (s *Store) SetRole(name string, role Role) error {
	if !ValidRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return fmt.Errorf("user %q: %w", name, ErrNotFound)
	}
	prev := u.Role
	u.Role = role
	if err := s.saveUsers(); err != nil {
		u.Role = prev
		return err
	}
	return nil
}

// DeleteUser removes an account. The last admin cannot be removed, since that
// would leave the instance unadministrable.
func (s *Store) DeleteUser(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return fmt.Errorf("user %q: %w", name, ErrNotFound)
	}
	if u.Role == RoleAdmin && s.countAdmins() == 1 {
		return fmt.Errorf("refusing to delete the last admin account")
	}
	delete(s.users, name)
	if err := s.saveUsers(); err != nil {
		s.users[name] = u
		return err
	}
	return nil
}

// countAdmins counts accounts that can actually administer the instance. A
// pending account cannot, so it must not be what keeps the last real admin
// deletable.
func (s *Store) countAdmins() int {
	n := 0
	for _, u := range s.users {
		if u.Role == RoleAdmin && !u.Pending {
			n++
		}
	}
	return n
}

// Authenticate returns the user if the password matches. The password is
// verified even for unknown usernames so that response time does not reveal
// which accounts exist.
func (s *Store) Authenticate(name, password string) (*User, bool) {
	s.mu.RLock()
	u, ok := s.users[name]
	s.mu.RUnlock()
	if !ok {
		verifyPassword("pbkdf2-sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
		return nil, false
	}
	if !verifyPassword(u.Hash, password) {
		return nil, false
	}
	return u, true
}

// User looks up an account by name.
func (s *Store) User(name string) (*User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[name]
	return u, ok
}

// Users returns all accounts, ordered by name.
func (s *Store) Users() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// UserCount reports how many accounts exist.
func (s *Store) UserCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

// SessionKey is the value a session cookie is bound to. Including the password
// hash means changing a password logs out that user everywhere.
func (u *User) SessionKey() string { return u.Name + "\x00" + u.Hash }
