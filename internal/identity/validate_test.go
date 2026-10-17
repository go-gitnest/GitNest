package identity

import (
	"strings"
	"testing"
)

func TestFoldNameIsASCIIOnly(t *testing.T) {
	for _, test := range []struct {
		name string
		want string
	}{
		{name: "Already-Lower", want: "already-lower"},
		{name: "MiXeD", want: "mixed"},
		{name: "digits-and-hyphens-42", want: "digits-and-hyphens-42"},
		{name: "", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := FoldName(test.name); got != test.want {
				t.Errorf("FoldName(%q) = %q, want %q", test.name, got, test.want)
			}
		})
	}
}

// TestFoldNameLeavesNonASCIIAlone pins that folding is ASCII-only, so it agrees
// with the database's lower() on the ASCII names the grammar allows and never
// depends on a locale.
func TestFoldNameLeavesNonASCIIAlone(t *testing.T) {
	const input = "Ünicode"
	if got := FoldName(input); got != input {
		t.Errorf("FoldName(%q) = %q, want it unchanged", input, got)
	}
}

func TestValidateUsername(t *testing.T) {
	for _, test := range []struct {
		username string
		wantErr  bool
	}{
		{username: "a", wantErr: false},
		{username: "alice", wantErr: false},
		{username: "Alice", wantErr: false},
		{username: "alice-2", wantErr: false},
		{username: "A1", wantErr: false},
		{username: strings.Repeat("a", MaxUsernameLength), wantErr: false},

		{username: "", wantErr: true},
		{username: strings.Repeat("a", MaxUsernameLength+1), wantErr: true},
		{username: "-alice", wantErr: true},
		{username: "alice-", wantErr: true},
		{username: "al--ice", wantErr: true},
		{username: "al ice", wantErr: true},
		{username: "al.ice", wantErr: true},
		{username: "al_ice", wantErr: true},
		{username: "alice@example.com", wantErr: true},
		{username: ".alice", wantErr: true},
		{username: "..", wantErr: true},
		{username: "登录", wantErr: true},
		{username: "Ünicode", wantErr: true},
		{username: "alice\x00", wantErr: true},

		// Reserved by docs/architecture/urls-and-identifiers.md section 6.9.
		{username: "admin", wantErr: true},
		{username: "ADMIN", wantErr: true},
		{username: "api", wantErr: true},
		{username: "login", wantErr: true},
		{username: "settings", wantErr: true},
		{username: "gitnest", wantErr: true},
		{username: "lfs", wantErr: true},
	} {
		t.Run(test.username, func(t *testing.T) {
			err := ValidateUsername(test.username)
			if test.wantErr && err == nil {
				t.Fatalf("ValidateUsername(%q) accepted the name", test.username)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("ValidateUsername(%q): %v", test.username, err)
			}
		})
	}
}

func TestReservedIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{"admin", "Admin", "ADMIN", "aDmIn"} {
		if !Reserved(name) {
			t.Errorf("Reserved(%q) = false, want true", name)
		}
	}
	if Reserved("notreserved") {
		t.Error("Reserved(\"notreserved\") = true, want false")
	}
}

func TestValidateEmail(t *testing.T) {
	for _, test := range []struct {
		email   string
		wantErr bool
	}{
		{email: "alice@example.com", wantErr: false},
		{email: "alice+git@example.co.uk", wantErr: false},
		{email: "a@b.co", wantErr: false},
		{email: "first.last@sub.example.com", wantErr: false},

		{email: "", wantErr: true},
		{email: "alice", wantErr: true},
		{email: "alice@example", wantErr: true},
		{email: "alice@@example.com", wantErr: true},
		{email: "@example.com", wantErr: true},
		{email: "alice@", wantErr: true},
		{email: "alice@.com", wantErr: true},
		{email: "alice@example..com", wantErr: true},
		{email: "alice@example.com.", wantErr: true},
		{email: "alice@-example.com", wantErr: true},
		{email: "alice@example-.com", wantErr: true},
		{email: "al ice@example.com", wantErr: true},
		{email: "alice@" + strings.Repeat("a", MaxEmailLength) + ".com", wantErr: true},
		{email: strings.Repeat("a", MaxEmailLocalPart+1) + "@example.com", wantErr: true},
	} {
		t.Run(test.email, func(t *testing.T) {
			err := ValidateEmail(test.email)
			if test.wantErr && err == nil {
				t.Fatalf("ValidateEmail(%q) accepted the address", test.email)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("ValidateEmail(%q): %v", test.email, err)
			}
		})
	}
}

func TestNormalizeEmailLowercasesAndTrims(t *testing.T) {
	if got := NormalizeEmail("  Alice@Example.COM  "); got != "alice@example.com" {
		t.Errorf("NormalizeEmail = %q, want the trimmed lowercase form", got)
	}
}

func TestValidatePassword(t *testing.T) {
	for _, test := range []struct {
		name     string
		password string
		wantErr  bool
	}{
		{name: "long passphrase", password: "correct horse battery staple", wantErr: false},
		{name: "exactly the minimum", password: strings.Repeat("x", MinPasswordLength), wantErr: false},
		{name: "one below the minimum", password: strings.Repeat("x", MinPasswordLength-1), wantErr: true},
		{name: "empty", password: "", wantErr: true},
		{name: "only whitespace", password: strings.Repeat(" ", MinPasswordLength), wantErr: true},
		{name: "one byte over the ceiling", password: strings.Repeat("x", MaxPasswordBytes+1), wantErr: true},
		{name: "exactly the ceiling", password: strings.Repeat("x", MaxPasswordBytes), wantErr: false},
		{name: "unicode counts by character", password: strings.Repeat("密", MinPasswordLength), wantErr: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePassword(test.password)
			if test.wantErr && err == nil {
				t.Fatal("ValidatePassword accepted the password")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("ValidatePassword: %v", err)
			}
		})
	}
}

// TestPasswordErrorNamesTheMinimum keeps the operator message actionable.
func TestPasswordErrorNamesTheMinimum(t *testing.T) {
	err := ValidatePassword("short")
	if err == nil {
		t.Fatal("a short password was accepted")
	}
	if !strings.Contains(err.Error(), "at least 8 characters") {
		t.Errorf("error = %v, want it to state the minimum", err)
	}
}
