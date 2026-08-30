package main

import (
	"strings"
	"testing"
)

func TestAPasswordIsNeverStoredInClear(t *testing.T) {
	stored, err := hashPassword("a good long password")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "a good long password") {
		t.Fatal("the password is in the stored form")
	}
	if !strings.HasPrefix(stored, "pbkdf2-sha256$") {
		t.Errorf("the stored form does not say how it was made: %q", stored)
	}
	if !passwordMatches(stored, "a good long password") {
		t.Error("the right password was refused")
	}
	if passwordMatches(stored, "a good long passworD") {
		t.Error("a wrong password was accepted")
	}
	// Two hashes of the same password differ: each carries its own salt.
	again, _ := hashPassword("a good long password")
	if again == stored {
		t.Error("two hashes of one password are identical — no salt")
	}
	for _, junk := range []string{"", "nonsense", "pbkdf2-sha256$notanumber$x$y", "md5$1$x$y"} {
		if passwordMatches(junk, "anything") {
			t.Errorf("%q was accepted as a hash", junk)
		}
	}
}

// The table is the boundary, so it is walked rather than trusted. An action
// nobody has classified belongs to the admin: a new one must not become
// everybody's by omission.
func TestWhatAnOperatorMayDo(t *testing.T) {
	operatorMay := []string{"reimage", "cancel", "reset", "shutdown", "reboot",
		"identify", "identify_off", "stealth_on", "stealth_off", "probe"}
	adminOnly := []string{"wipe", "something_invented_next_year"}

	for _, kind := range operatorMay {
		if !mayDo(roleOperator, kind) {
			t.Errorf("an operator may not %q and should", kind)
		}
		if !mayDo(roleAdmin, kind) {
			t.Errorf("an admin may not %q, and an admin may everything", kind)
		}
	}
	for _, kind := range adminOnly {
		if mayDo(roleOperator, kind) {
			t.Errorf("an operator may %q and should not", kind)
		}
		if !mayDo(roleAdmin, kind) {
			t.Errorf("an admin may not %q", kind)
		}
	}
}

func TestAccountsComeAndGoButNotTheLastAdmin(t *testing.T) {
	a := testApp(t)

	if err := a.createUser("ada", "correct horse battery", roleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := a.createUser("olive", "another long secret", roleOperator); err != nil {
		t.Fatal(err)
	}
	if err := a.createUser("ada", "duplicate", roleAdmin); err == nil {
		t.Error("the same name was taken twice")
	}
	for _, bad := range []struct{ name, pw string }{
		{"a", "long enough password"},
		{"has space", "long enough password"},
		{"fine", "short"},
	} {
		if err := a.createUser(bad.name, bad.pw, roleOperator); err == nil {
			t.Errorf("%q / %q was accepted", bad.name, bad.pw)
		}
	}

	if u, ok := a.authenticate("ada", "correct horse battery"); !ok || u.Role != roleAdmin {
		t.Error("the admin could not sign in")
	}
	if _, ok := a.authenticate("ada", "wrong"); ok {
		t.Error("a wrong password signed in")
	}
	if _, ok := a.authenticate("nobody", "wrong"); ok {
		t.Error("an account that does not exist signed in")
	}

	// Ada is the only admin: she cannot be demoted, disabled or removed.
	if err := a.setUserRole("ada", roleOperator); err == nil {
		t.Error("the last admin was demoted")
	}
	if err := a.setUserDisabled("ada", true); err == nil {
		t.Error("the last admin was disabled")
	}
	if err := a.deleteUser("ada"); err == nil {
		t.Error("the last admin was removed")
	}

	// With a second admin, all three become possible.
	if err := a.setUserRole("olive", roleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := a.deleteUser("ada"); err != nil {
		t.Errorf("with two admins, one could not be removed: %v", err)
	}

	// A disabled account is refused even with the right password.
	if err := a.createUser("stopped", "yet another secret", roleOperator); err != nil {
		t.Fatal(err)
	}
	if err := a.setUserDisabled("stopped", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.authenticate("stopped", "yet another secret"); ok {
		t.Error("a disabled account signed in")
	}
}
