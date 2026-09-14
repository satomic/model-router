// Package localadmin holds the local super administrator's credential: hashing,
// verification, and the forced-change rule.
//
// This account exists so the console stays reachable where GitHub is not -- an air-gapped
// network, a blocked github.com, a misconfigured OAuth app. It is a genuine super
// administrator (it sees every user's traces and may delete them), so the credential gets
// treated accordingly:
//
//   - The password is stored salted with scrypt, never with the bare unsalted sha256 that
//     hashes API keys. That is right for a 256-bit random key and wrong for a human-chosen
//     password.
//   - Until the password has actually been changed, the default one is in force and the
//     session is refused everywhere except the change-password form.
//   - The hash lives in config.yaml alongside every other secret, and that file is gitignored.
package localadmin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	DefaultUsername = "admin"
	// Documented in docs/authentication.md and in config.example.yaml. It is only ever enough
	// to reach the change-password form, never the console itself.
	DefaultPassword   = "admin1234"
	MinPasswordLength = 8
)

// Cost parameters. N=2**14 keeps a sign-in comfortably under ~100ms on a laptop while
// making an offline guessing run against the stored hash expensive.
const (
	scryptN = 1 << 14
	scryptR = 8
	scryptP = 1
	dkLen   = 32
)

// HashPassword returns (saltHex, hashHex). A fresh random salt unless one is supplied.
func HashPassword(password, saltHex string) (string, string, error) {
	var salt []byte
	if saltHex != "" {
		decoded, err := hex.DecodeString(saltHex)
		if err != nil {
			return "", "", err
		}
		salt = decoded
	} else {
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return "", "", err
		}
	}
	digest, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, dkLen)
	if err != nil {
		return "", "", err
	}
	return hex.EncodeToString(salt), hex.EncodeToString(digest), nil
}

// Settings is the subset of auth.local_admin this package reads. The config package
// supplies it, so nothing here has to import the configuration and create a cycle.
type Settings struct {
	Enabled      bool
	Username     string
	PasswordHash string
	PasswordSalt string
}

func (s Settings) hasStoredPassword() bool {
	return strings.TrimSpace(s.PasswordHash) != "" && strings.TrimSpace(s.PasswordSalt) != ""
}

// MustChange reports whether the built-in default password is still in force.
//
// Blanking the hash fields in config.yaml therefore restores the default password -- the
// deliberate lockout-recovery path for an operator who has lost the new one.
func MustChange(s Settings) bool {
	return s.Enabled && !s.hasStoredPassword()
}

// VerifyPassword is a constant-time check against the stored hash, or against the default
// password while no hash has been stored yet.
func VerifyPassword(s Settings, password string) bool {
	if !s.Enabled {
		return false
	}
	if !s.hasStoredPassword() {
		return subtle.ConstantTimeCompare([]byte(password), []byte(DefaultPassword)) == 1
	}
	_, candidate, err := HashPassword(password, s.PasswordSalt)
	if err != nil { // a corrupt / hand-edited salt
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.PasswordHash)) == 1
}

// ValidateNewPassword returns an error message for an unacceptable password, or "".
func ValidateNewPassword(password string) string {
	if len(password) < MinPasswordLength {
		return fmt.Sprintf("password must be at least %d characters", MinPasswordLength)
	}
	if password == DefaultPassword {
		// Otherwise "change the password" could be satisfied by re-entering the documented
		// one, which would leave the account exactly as exposed as before.
		return "choose a password other than the default one"
	}
	return ""
}

// ValidateUsername returns an error message for an unacceptable username, or "".
func ValidateUsername(username string) string {
	name := strings.TrimSpace(username)
	if name == "" {
		return "username must not be empty"
	}
	if len(name) > 64 {
		return "username must be at most 64 characters"
	}
	return ""
}
