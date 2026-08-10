// Package auth implements local authentication: Argon2id password hashing,
// server-side sessions, CSRF protection and login rate limiting.
//
// Plain-text passwords are never stored, never logged and never compared with
// anything other than a constant-time function.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2 parameters.
//
// These target roughly 64 MiB and a few hundred milliseconds on a modern
// machine — deliberately expensive, because the whole point is to make an
// offline guessing attack against a stolen hash uneconomic. The parameters are
// stored in each hash, so raising them later does not invalidate existing
// passwords: an old hash still verifies with its own parameters.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16

	// Stored hashes are data, not trusted instructions. These generous ceilings
	// prevent a corrupt or attacker-modified database value from turning login
	// into an extreme allocation or CPU request while leaving room for upgrades.
	maxEncodedHashLen          = 512
	maxArgonMemory      uint32 = 256 * 1024 // KiB
	maxArgonTime        uint32 = 10
	maxArgonParallelism        = 16
	maxArgonSaltLen            = 64
	maxArgonKeyLen             = 64
)

// Password policy.
const (
	// MinPasswordLength is deliberately generous rather than demanding
	// character classes: length beats composition rules for real strength.
	MinPasswordLength = 12
	MaxPasswordLength = 1024
	MinUsernameLength = 3
	MaxUsernameLength = 64
)

// Errors returned by password handling.
var (
	ErrPasswordTooShort  = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	ErrPasswordTooLong   = fmt.Errorf("password must be at most %d characters", MaxPasswordLength)
	ErrInvalidHashFormat = errors.New("stored password hash is not in the expected format")
	ErrIncompatibleHash  = errors.New("stored password hash uses an unsupported algorithm version")
	ErrInvalidUsername   = errors.New("username must be 3-64 characters using letters, digits, dot, dash or underscore")
)

// argonParallelism picks a lane count from the machine's CPUs, bounded so that
// a hash produced on a large host still verifies affordably on a small one.
func argonParallelism() uint8 {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	return uint8(n)
}

// HashPassword returns an encoded Argon2id hash in the standard PHC string
// format, which embeds the algorithm, its parameters and the salt.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	par := argonParallelism()
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, par, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, par,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword checks a candidate password against an encoded hash.
//
// The comparison is constant-time so that verification duration cannot reveal
// how much of the hash matched.
func VerifyPassword(password, encoded string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	if len(password) > MaxPasswordLength {
		return false, ErrPasswordTooLong
	}

	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	memory      uint32
	time        uint32
	parallelism uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	var p argonParams
	if len(encoded) == 0 || len(encoded) > maxEncodedHashLen {
		return p, nil, nil, ErrInvalidHashFormat
	}

	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, key
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, ErrInvalidHashFormat
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, ErrIncompatibleHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHashFormat
	}
	if version != argon2.Version {
		return p, nil, nil, ErrIncompatibleHash
	}

	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return p, nil, nil, ErrInvalidHashFormat
	}
	for _, item := range kv {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			return p, nil, nil, ErrInvalidHashFormat
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return p, nil, nil, ErrInvalidHashFormat
		}
		switch k {
		case "m":
			p.memory = uint32(n)
		case "t":
			p.time = uint32(n)
		case "p":
			if n == 0 || n > 255 {
				return p, nil, nil, ErrInvalidHashFormat
			}
			p.parallelism = uint8(n)
		default:
			return p, nil, nil, ErrInvalidHashFormat
		}
	}
	// Reject nonsensical parameters that would make verification trivial or
	// impossibly expensive.
	if p.memory < 1024 || p.memory > maxArgonMemory ||
		p.time == 0 || p.time > maxArgonTime ||
		p.parallelism == 0 || p.parallelism > maxArgonParallelism {
		return p, nil, nil, ErrInvalidHashFormat
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < argonSaltLen || len(salt) > maxArgonSaltLen {
		return p, nil, nil, ErrInvalidHashFormat
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > maxArgonKeyLen {
		return p, nil, nil, ErrInvalidHashFormat
	}
	return p, salt, key, nil
}

// ValidatePassword enforces the password policy.
func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if len(password) > MaxPasswordLength {
		return ErrPasswordTooLong
	}
	return nil
}

// ValidateUsername enforces the username policy and returns the canonical
// lowercase form.
func ValidateUsername(username string) (string, error) {
	u := strings.ToLower(strings.TrimSpace(username))
	if len(u) < MinUsernameLength || len(u) > MaxUsernameLength {
		return "", ErrInvalidUsername
	}
	for _, r := range u {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
		default:
			return "", ErrInvalidUsername
		}
	}
	return u, nil
}

// dummyHash is a real Argon2id hash of a random value, used to spend
// comparable time when a username does not exist.
//
// Without it, a failed lookup would return noticeably faster than a wrong
// password, and the response time would disclose which usernames are
// registered.
var dummyHash = func() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	h, err := HashPassword(base64.RawStdEncoding.EncodeToString(buf))
	if err != nil {
		// Only reachable if the policy rejects a 43-character random string.
		return "$argon2id$v=19$m=65536,t=3,p=2$YWJjZGVmZ2hpamtsbW5vcA$" +
			"c29tZWZpeGVkdmFsdWV0aGF0d2lsbG5ldmVybWF0Y2g"
	}
	return h
}()

// SpendDummyVerification performs a password verification against a throwaway
// hash. Callers use it on the "no such user" path so that both outcomes cost
// roughly the same.
func SpendDummyVerification(password string) {
	_, _ = VerifyPassword(password, dummyHash)
}
