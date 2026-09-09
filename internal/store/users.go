package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
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
	// SessionsFrom is the instant before which session cookies for this
	// account are no longer honoured. Bumping it is how a stateless design
	// revokes: there is no session table to delete a row from.
	SessionsFrom time.Time `json:"sessions_from,omitzero"`
}

// SessionKey is the value a session cookie is bound to.
//
// The password hash is in it, so changing a password invalidates every cookie.
// So is SessionsFrom, which gives signing out something to actually do: without
// it, "Sign out" only cleared the cookie, and a copy captured beforehand stayed
// valid for the rest of its twelve hours.
//
// The cost is that signing out signs this account out on every device. With no
// session table there is nothing finer to revoke, and for an administrative
// account that is the safer default anyway.
func (u *User) SessionKey() string {
	return u.Name + "\x00" + u.Hash + "\x00" + dbTime(u.SessionsFrom)
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

const userColumns = `name, role, hash, created, must_change_password, pending, invited_by, sessions_from`

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(row rowScanner) (*User, error) {
	var u User
	var created string
	var sessionsFrom sql.NullString
	if err := row.Scan(&u.Name, &u.Role, &u.Hash, &created,
		&u.MustChangePassword, &u.Pending, &u.InvitedBy, &sessionsFrom); err != nil {
		return nil, err
	}
	u.Created = goTime(created)
	u.SessionsFrom = goTimePtr(sessionsFrom)
	return &u, nil
}

// AddUser creates an approved account. It fails if the name is taken.
func (s *Store) AddUser(name string, role Role, password string) (*User, error) {
	return s.addUser(name, role, password, false, false, "")
}

// AddUserMustChange creates an account required to set a new password before it
// can do anything else. Used for the bootstrap administrator.
func (s *Store) AddUserMustChange(name string, role Role, password string) (*User, error) {
	return s.addUser(name, role, password, true, false, "")
}

// validUsername keeps names to characters that survive every place a name is
// used. "|" in particular is the separator in a session cookie, so an account
// containing one could be created and could never sign in.
var validUsername = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidUsername reports whether a name is acceptable.
func ValidUsername(name string) bool { return validUsername.MatchString(name) }

func (s *Store) addUser(name string, role Role, password string, mustChange, pending bool, invitedBy string) (*User, error) {
	name = strings.TrimSpace(name)
	if !ValidUsername(name) {
		return nil, fmt.Errorf("a username must be 1 to 64 characters of letters, " +
			"digits, dot, dash or underscore, starting with a letter or digit")
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
	u := &User{
		Name: name, Role: role, Hash: hash, Created: time.Now().UTC(),
		MustChangePassword: mustChange, Pending: pending, InvitedBy: invitedBy,
	}
	_, err = s.db.Exec(
		`INSERT INTO users (`+userColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Name, u.Role, u.Hash, dbTime(u.Created), u.MustChangePassword, u.Pending,
		u.InvitedBy, nil)
	if err != nil {
		if isUnique(err) {
			return nil, fmt.Errorf("user %q: %w", name, ErrExists)
		}
		return nil, err
	}
	return u, nil
}

// SetPassword replaces a user's password, which also invalidates their existing
// sessions, since a session MAC covers the password hash.
func (s *Store) SetPassword(name, password string) error {
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE users SET hash = ?, must_change_password = 0 WHERE name = ?`, hash, name)
	return affectedOne(res, err, name)
}

// RevokeSessions invalidates every session cookie this account holds, by moving
// the instant they are bound to. Used by sign-out.
func (s *Store) RevokeSessions(name string) error {
	res, err := s.db.Exec(`UPDATE users SET sessions_from = ? WHERE name = ?`,
		dbTime(time.Now().UTC()), name)
	return affectedOne(res, err, name)
}

// SetRole changes a user's role. It refuses to demote the last administrator,
// for the same reason DeleteUser refuses to remove them: an instance nobody can
// administer is unrecoverable without shell access.
func (s *Store) SetRole(name string, role Role) error {
	if !ValidRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	return s.tx(func(tx *sql.Tx) error {
		var current Role
		var pending bool
		err := tx.QueryRow(`SELECT role, pending FROM users WHERE name = ?`, name).Scan(&current, &pending)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("user %q: %w", name, ErrNotFound)
		} else if err != nil {
			return err
		}
		if current == RoleAdmin && !pending && role != RoleAdmin {
			var others int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM users WHERE role = ? AND pending = 0 AND name != ?`,
				RoleAdmin, name).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return fmt.Errorf("refusing to demote the last admin account")
			}
		}
		_, err = tx.Exec(`UPDATE users SET role = ? WHERE name = ?`, role, name)
		return err
	})
}

// DeleteUser removes an account. The last administrator cannot be removed,
// since that would leave the instance unadministrable.
func (s *Store) DeleteUser(name string) error {
	return s.tx(func(tx *sql.Tx) error {
		var role Role
		var pending bool
		err := tx.QueryRow(`SELECT role, pending FROM users WHERE name = ?`, name).Scan(&role, &pending)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("user %q: %w", name, ErrNotFound)
		} else if err != nil {
			return err
		}
		if role == RoleAdmin && !pending {
			var others int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM users WHERE role = ? AND pending = 0 AND name != ?`,
				RoleAdmin, name).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return fmt.Errorf("refusing to delete the last admin account")
			}
		}
		_, err = tx.Exec(`DELETE FROM users WHERE name = ?`, name)
		return err
	})
}

// Authenticate returns the user if the password matches. The password is
// verified even for unknown usernames, so response time does not reveal which
// accounts exist.
func (s *Store) Authenticate(name, password string) (*User, bool) {
	u, ok := s.User(name)
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
	u, err := scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE name = ?`, name))
	if err != nil {
		return nil, false
	}
	return u, true
}

// Users returns all accounts, ordered by name.
func (s *Store) Users() []*User { return s.usersWhere(`ORDER BY name`) }

// PendingUsers returns accounts awaiting approval, oldest first.
func (s *Store) PendingUsers() []*User {
	return s.usersWhere(`WHERE pending = 1 ORDER BY created`)
}

func (s *Store) usersWhere(clause string) []*User {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ` + clause)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return out
		}
		out = append(out, u)
	}
	return out
}

// UserCount reports how many accounts exist.
func (s *Store) UserCount() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

// ApproveUser clears the pending flag and sets the account's role.
func (s *Store) ApproveUser(name string, role Role) error {
	if !ValidRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	return s.tx(func(tx *sql.Tx) error {
		var pending bool
		err := tx.QueryRow(`SELECT pending FROM users WHERE name = ?`, name).Scan(&pending)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("user %q: %w", name, ErrNotFound)
		} else if err != nil {
			return err
		}
		if !pending {
			return fmt.Errorf("%q is already approved", name)
		}
		_, err = tx.Exec(`UPDATE users SET pending = 0, role = ? WHERE name = ?`, role, name)
		return err
	})
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

func affectedOne(res sql.Result, err error, name string) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("user %q: %w", name, ErrNotFound)
	}
	return nil
}
