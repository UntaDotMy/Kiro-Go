package config

import "testing"

// TestPasswordHashedAtRest verifies SetPassword stores a bcrypt hash, not
// plaintext, and that VerifyPassword still matches the original.
func TestPasswordHashedAtRest(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{})

	SetPassword("s3cret-admin-pw")

	stored := GetPassword()
	if stored == "s3cret-admin-pw" {
		t.Fatal("password stored as plaintext — must be bcrypt-hashed at rest")
	}
	if !looksLikeBcrypt(stored) {
		t.Fatalf("stored password is not a bcrypt hash: %q", stored)
	}
	if !VerifyPassword("s3cret-admin-pw") {
		t.Fatal("VerifyPassword must accept the correct password against the hash")
	}
	if VerifyPassword("wrong") {
		t.Fatal("VerifyPassword must reject a wrong password")
	}
}

// TestVerifyPasswordLegacyPlaintext verifies a pre-hash config (plaintext
// password stored directly) still authenticates, so upgrades don't lock out.
func TestVerifyPasswordLegacyPlaintext(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{Password: "legacy-plain"})

	if !VerifyPassword("legacy-plain") {
		t.Fatal("VerifyPassword must accept a legacy plaintext password")
	}
	if VerifyPassword("nope") {
		t.Fatal("VerifyPassword must reject a wrong password against plaintext")
	}
}

// TestIsDefaultPassword verifies the startup-guard predicate works for both the
// bundled plaintext default and a hashed copy of it.
func TestIsDefaultPassword(t *testing.T) {
	// Plaintext default (fresh-install shape).
	withTestAPIKeyConfig(t, &Config{Password: DefaultAdminPassword})
	if !IsDefaultPassword() {
		t.Fatal("IsDefaultPassword must detect the plaintext bundled default")
	}

	// Hashed default — the guard must still detect it.
	withTestAPIKeyConfig(t, &Config{Password: hashPasswordForStorage(DefaultAdminPassword)})
	if !IsDefaultPassword() {
		t.Fatal("IsDefaultPassword must detect a hashed copy of the default")
	}

	// A real password must NOT read as default.
	withTestAPIKeyConfig(t, &Config{Password: hashPasswordForStorage("a-real-password")})
	if IsDefaultPassword() {
		t.Fatal("a non-default password must not read as default")
	}
}

// TestHashPasswordIdempotent verifies hashing an already-hashed value is a
// no-op (so re-saving config doesn't double-hash and lock the operator out).
func TestHashPasswordIdempotent(t *testing.T) {
	h1 := hashPasswordForStorage("pw")
	h2 := hashPasswordForStorage(h1)
	if h1 != h2 {
		t.Fatal("hashing an already-bcrypt value must be a no-op")
	}
	// Empty stays empty (no password set).
	if got := hashPasswordForStorage(""); got != "" {
		t.Fatalf("empty password must stay empty, got %q", got)
	}
}
