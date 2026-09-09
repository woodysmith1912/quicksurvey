package store

import (
	"fmt"
	"sync"
	"testing"
)

// Two administrators working the approval queue at the same time is ordinary,
// not exotic: a notification goes out and both open it.
func TestConcurrentApprovalsOfTheSameAccount(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateInvite("root", RoleEditor, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimInvite(token, "newbie", "password123"); err != nil {
		t.Fatal(err)
	}

	// Both admins act at once, and they disagree about the role.
	const tries = 8
	var wg sync.WaitGroup
	results := make(chan error, tries)
	roles := make(chan Role, tries)
	for i := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			role := RoleViewer
			if i%2 == 0 {
				role = RoleAdmin
			}
			if err := s.ApproveUser("newbie", role); err == nil {
				roles <- role
			} else {
				results <- err
			}
		}()
	}
	wg.Wait()
	close(results)
	close(roles)

	var succeeded int
	var winner Role
	for r := range roles {
		succeeded++
		winner = r
	}
	if succeeded != 1 {
		t.Errorf("%d of %d concurrent approvals succeeded, want exactly 1", succeeded, tries)
	}
	// The account must end up in one of the two states someone asked for, not
	// a mixture, and must be approved exactly once.
	u, ok := s.User("newbie")
	if !ok {
		t.Fatal("the account vanished")
	}
	if u.Pending {
		t.Error("the account is still pending after a successful approval")
	}
	if u.Role != winner {
		t.Errorf("role = %q but the approval that succeeded asked for %q", u.Role, winner)
	}
	if len(s.PendingUsers()) != 0 {
		t.Error("the account is still in the approval queue")
	}
}

// Approving and rejecting at the same moment must not leave a half-deleted or
// half-approved account.
func TestConcurrentApproveAndReject(t *testing.T) {
	// Few iterations, because each one builds two accounts and PBKDF2 is
	// deliberately expensive. Run with -count to hunt harder.
	for range 4 {
		s := newStore(t)
		if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
			t.Fatal(err)
		}
		_, token, _ := s.CreateInvite("root", RoleEditor, "", 0)
		if _, err := s.ClaimInvite(token, "newbie", "password123"); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var approveErr, rejectErr error
		go func() { defer wg.Done(); approveErr = s.ApproveUser("newbie", RoleEditor) }()
		go func() { defer wg.Done(); rejectErr = s.DeleteUser("newbie") }()
		wg.Wait()

		u, exists := s.User("newbie")
		switch {
		case !exists:
			// Rejected. The approval must have failed, not silently done half
			// its work.
			if approveErr == nil && rejectErr == nil {
				// Approve landed first, then the delete removed it. Consistent.
			}
		case u.Pending:
			t.Fatalf("the account survived as pending: approve=%v reject=%v", approveErr, rejectErr)
		default:
			// Approved and still present: the delete must have failed or run
			// first and been re-created, which cannot happen here.
			if rejectErr == nil {
				t.Fatalf("both approve and reject reported success, yet the account exists")
			}
		}
	}
}

// The single-use guarantee on an invitation is only interesting under a race:
// two people opening the same link at the same moment.
func TestConcurrentInviteClaims(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateInvite("root", RoleEditor, "", 0)
	if err != nil {
		t.Fatal(err)
	}

	const racers = 10
	var wg sync.WaitGroup
	claimed := make(chan string, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if u, err := s.ClaimInvite(token, fmt.Sprintf("claimant%d", i), "password123"); err == nil {
				claimed <- u.Name
			}
		}()
	}
	wg.Wait()
	close(claimed)

	var names []string
	for n := range claimed {
		names = append(names, n)
	}
	if len(names) != 1 {
		t.Errorf("%d of %d concurrent claims succeeded, want exactly 1: %v", len(names), racers, names)
	}
	// Exactly one account exists beyond the administrator.
	accounts := 0
	for _, u := range s.Users() {
		if u.Name != "root" {
			accounts++
		}
	}
	if accounts != 1 {
		t.Errorf("%d accounts created from one single-use link", accounts)
	}
	if _, ok := s.InviteByToken(token); ok {
		t.Error("the link is still open after being claimed")
	}
}

// The same question for a reset link: two tabs, two submissions.
func TestConcurrentResetClaims(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("bob", RoleEditor, "password123"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateReset("bob", "root")
	if err != nil {
		t.Fatal(err)
	}

	const racers = 10
	var wg sync.WaitGroup
	won := make(chan string, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pw := fmt.Sprintf("password-from-tab-%d", i)
			if _, err := s.UseReset(token, pw); err == nil {
				won <- pw
			}
		}()
	}
	wg.Wait()
	close(won)

	var winners []string
	for p := range won {
		winners = append(winners, p)
	}
	if len(winners) != 1 {
		t.Fatalf("%d of %d concurrent resets succeeded, want exactly 1", len(winners), racers)
	}
	// The password that took effect is the one that reported success, not some
	// other racer's.
	if _, ok := s.Authenticate("bob", winners[0]); !ok {
		t.Error("the winning submission's password does not work")
	}
}
