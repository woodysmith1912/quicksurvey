package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInviteRoundTrip(t *testing.T) {
	s := newStore(t)
	iv, token, err := s.CreateInvite("root", RoleEditor, "join the team", 0)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if token == "" || len(token) < 20 {
		t.Fatalf("token = %q, want a long random string", token)
	}

	// The secret must not be recoverable from what is stored. Check the
	// database bytes directly rather than trusting the schema.
	raw, err := os.ReadFile(filepath.Join(s.Dir(), dbFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Error("the database contains the raw invitation token; only a digest should be stored")
	}

	got, ok := s.InviteByToken(token)
	if !ok {
		t.Fatal("a fresh invite should resolve")
	}
	if got.ID != iv.ID || got.Role != RoleEditor {
		t.Errorf("resolved %+v, want id %s role editor", got, iv.ID)
	}
	if _, ok := s.InviteByToken("not-a-real-token"); ok {
		t.Error("a bogus token resolved")
	}
	if _, ok := reopen(t, s).InviteByToken(token); !ok {
		t.Error("the invite did not survive a restart")
	}
}

func TestInviteIsSingleUse(t *testing.T) {
	s := newStore(t)
	_, token, err := s.CreateInvite("root", RoleEditor, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimInvite(token, "alice", "password123"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := s.ClaimInvite(token, "mallory", "password123"); err == nil {
		t.Fatal("the same link produced a second account")
	}
	if _, ok := s.User("mallory"); ok {
		t.Error("the second account was created anyway")
	}
	if _, ok := s.InviteByToken(token); ok {
		t.Error("a claimed invite still resolves as open")
	}
}

func TestClaimedAccountIsPendingAndPowerless(t *testing.T) {
	s := newStore(t)
	_, token, _ := s.CreateInvite("root", RoleAdmin, "", 0)
	u, err := s.ClaimInvite(token, "alice", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Pending {
		t.Fatal("a claimed account must start pending")
	}
	if u.InvitedBy != "root" {
		t.Errorf("InvitedBy = %q, want root", u.InvitedBy)
	}
	// The password still works — being pending is an authorisation state, not
	// a broken credential.
	if _, ok := s.Authenticate("alice", "password123"); !ok {
		t.Error("a pending account should still authenticate")
	}
	if got := s.PendingUsers(); len(got) != 1 || got[0].Name != "alice" {
		t.Errorf("PendingUsers = %v, want [alice]", got)
	}
}

// A pending admin must not satisfy the last-admin guarantee, or approving
// nobody would still allow the only real admin to be deleted.
func TestPendingAdminDoesNotCountAsAnAdmin(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	_, token, _ := s.CreateInvite("root", RoleAdmin, "", 0)
	if _, err := s.ClaimInvite(token, "alice", "password123"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser("root"); err == nil {
		t.Fatal("the last real admin was deletable while the only other admin was still pending")
	}
	if err := s.ApproveUser("alice", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser("root"); err != nil {
		t.Fatalf("after approving a second admin, deletion should work: %v", err)
	}
}

func TestApproveClearsPendingAndSetsRole(t *testing.T) {
	s := newStore(t)
	_, token, _ := s.CreateInvite("root", RoleAdmin, "", 0)
	if _, err := s.ClaimInvite(token, "alice", "password123"); err != nil {
		t.Fatal(err)
	}
	// The invite suggested admin; the approver may decide otherwise.
	if err := s.ApproveUser("alice", RoleViewer); err != nil {
		t.Fatal(err)
	}
	u, _ := s.User("alice")
	if u.Pending || u.Role != RoleViewer {
		t.Errorf("after approval: pending=%v role=%q, want false/viewer", u.Pending, u.Role)
	}
	if err := s.ApproveUser("alice", RoleViewer); err == nil {
		t.Error("approving an already-approved account should be refused")
	}
	if len(reopen(t, s).PendingUsers()) != 0 {
		t.Error("approval did not survive a restart")
	}
}

func TestExpiredAndRevokedInvitesAreRefused(t *testing.T) {
	s := newStore(t)
	iv, token, err := s.CreateInvite("root", RoleEditor, "", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.InviteByToken(token); ok {
		t.Error("an expired invite still resolves")
	}
	if _, err := s.ClaimInvite(token, "late", "password123"); err == nil {
		t.Error("an expired invite was claimable")
	}
	if got := iv.Status(time.Now()); got != "expired" {
		t.Errorf("Status = %q, want expired", got)
	}

	_, token2, _ := s.CreateInvite("root", RoleEditor, "", 0)
	list := s.Invites()
	var open *Invite
	for _, c := range list {
		if c.Open(time.Now()) {
			open = c
		}
	}
	if open == nil {
		t.Fatal("expected one open invite")
	}
	if err := s.RevokeInvite(open.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimInvite(token2, "nope", "password123"); err == nil {
		t.Error("a revoked invite was claimable")
	}
}

func TestClaimRejectsTakenUsernameAndShortPassword(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("taken", RoleViewer, "password123"); err != nil {
		t.Fatal(err)
	}
	_, token, _ := s.CreateInvite("root", RoleEditor, "", 0)
	if _, err := s.ClaimInvite(token, "taken", "password123"); err == nil {
		t.Error("claiming with an existing username should be refused")
	}
	if _, err := s.ClaimInvite(token, "newbie", "short"); err == nil {
		t.Error("a short password should be refused")
	}
	// Neither failure may spend the link.
	if _, ok := s.InviteByToken(token); !ok {
		t.Error("a failed claim consumed the invitation")
	}
}
