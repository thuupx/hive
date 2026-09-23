// Package ids generates Hive identifiers.
//
// Identifiers are for uniqueness, not ordering. A Hive event sequence is
// assigned by the event store and is never derived from an id.
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a short unique identifier with a readable prefix.
func New(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice. A degraded identifier is worse
		// than nothing, but a panic in a daemon would be worse still.
		return prefix + "_fallback"
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
