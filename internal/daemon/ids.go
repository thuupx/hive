// Package daemon composes the coordinator and node runtimes.
//
// This is the composition root. The coordinator wires the store, the event bus,
// the plugin supervisor, and the node server. The node wires the node store, the
// node link, and its agent plugins.
//
// Keeping composition here rather than in main makes the whole stack testable
// in process.
package daemon

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a short unique identifier with a readable prefix.
//
// Identifiers are for uniqueness, not ordering: a Hive event sequence is
// assigned by the event store, never derived from an id.
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "_fallback"
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
