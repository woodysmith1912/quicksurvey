package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResetLinkLetsTheOwnerChooseTheirOwnPassword(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("alice", RoleEditor, "old-password"); err != nil {
		t.Fatal(err)
	}
	rp, token, err := s.CreateReset("alice", "root")
	if err != nil {
		t.Fatal(err)
	}
	if rp.User != "alice" || rp.CreatedBy != "root" {
		t.Errorf("reset = %+v", rp)
	}
	// The admin who created it never learns the password that results.
	raw, err := os.ReadFile(filepath.Join(s.Dir(), dbFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Error("the database contains the raw reset token; only a digest should be stored")
	}

	before, _ := s.User("alice")
	user, err := s.UseReset(token, "a password only alice knows")
	if err != nil {
		t.Fatalf("UseReset: %v", err)
	}
	if user != "alice" {
		t.Errorf("UseReset returned %q", user)
	}
	if _, ok := s.Authenticate("alice", "a password only alice knows"); !ok {
		t.Error("the new password does not work")
	}
	if _, ok := s.Authenticate("alice", "old-password"); ok {
		t.Error("the old password still works")
	}
	// A reset is usually a response to something going wrong, so old cookies
	// must not survive it.
	after, _ := s.User("alice")
	if after.SessionKey() == before.SessionKey() {
		t.Error("existing sessions were not revoked by the reset")
	}
}

func TestResetLinkIsSingleUse(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("alice", RoleEditor, "old-password"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateReset("alice", "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseReset(token, "first choice here"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseReset(token, "second choice here"); err == nil {
		t.Fatal("the same link set a password twice")
	}
	if _, ok := s.Authenticate("alice", "second choice here"); ok {
		t.Error("the second use took effect anyway")
	}
	if _, ok := s.ResetByToken(token); ok {
		t.Error("a spent link still resolves")
	}
}

func TestNewResetLinkRetiresTheOldOne(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("alice", RoleEditor, "old-password"); err != nil {
		t.Fatal(err)
	}
	_, first, err := s.CreateReset("alice", "root")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := s.CreateReset("alice", "root")
	if err != nil {
		t.Fatal(err)
	}
	// Two live links for one account would be two ways in; issuing a second
	// must close the first.
	if _, ok := s.ResetByToken(first); ok {
		t.Error("the superseded link is still usable")
	}
	if _, ok := s.ResetByToken(second); !ok {
		t.Error("the newest link does not work")
	}
	if rp, ok := s.OutstandingReset("alice"); !ok || !rp.Open(time.Now()) {
		t.Error("the accounts page would not know a link is outstanding")
	}
}

func TestResetLinkExpiresAndRejectsShortPasswords(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("alice", RoleEditor, "old-password"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateReset("alice", "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseReset(token, "short"); err == nil {
		t.Error("a short password was accepted")
	}
	// A failed attempt must not spend the link.
	if _, ok := s.ResetByToken(token); !ok {
		t.Error("a rejected password consumed the link")
	}
	if _, _, err := s.CreateReset("nobody", "root"); err == nil {
		t.Error("a reset for a nonexistent account was created")
	}
}
