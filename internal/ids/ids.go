// Package ids generates and validates the identifiers used across Relata.
//
// Every persisted record uses a UUIDv4 string primary key. Chunk IDs are used
// verbatim as Qdrant point IDs, which Qdrant accepts in UUID form; that removes
// any need for a separate ID mapping table.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"

	"github.com/google/uuid"
)

// New returns a random UUIDv4 string.
func New() string { return uuid.NewString() }

// Valid reports whether s is a well-formed UUID. Used to reject client-supplied
// identifiers before they reach a database query.
func Valid(s string) bool {
	if len(s) != 36 {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}

// Token returns a URL-safe random string with n bytes of entropy, used for
// session and CSRF tokens.
func Token(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the hex SHA-256 of a token. Sessions store this rather than
// the token itself so that database contents cannot be replayed as credentials.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Deterministic derives a stable UUID from parts, used for idempotency keys and
// dedupe keys so that reprocessing replaces rather than duplicates records.
func Deterministic(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0}) // separator prevents ("ab","c") colliding with ("a","bc")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DeterministicUUID derives a stable UUIDv5-shaped identifier from parts. Used
// where a UUID is structurally required (Qdrant point IDs) but the value must be
// reproducible across runs.
func DeterministicUUID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	var u uuid.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u.String()
}
