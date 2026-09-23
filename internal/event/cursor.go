package event

import "errors"

// ErrCursorExpired reports that a requested event sequence has been pruned.
//
// The protocol does not claim that a historical cursor remains valid forever.
// A client that receives this must rehydrate from the snapshot boundary
// before continuing the stream, rather than silently skipping history.
var ErrCursorExpired = errors.New("event: cursor expired")

// CursorGap describes how a client recovers from a pruned cursor.
//
// NextSequence is the lowest sequence still available. SnapshotID and
// SnapshotSequence identify the latest available context boundary; they are
// zero when the session has no snapshot, in which case the client resumes
// from NextSequence without rehydrating.
//
// A client that rehydrates a snapshot ahead of NextSequence resumes from
// SnapshotSequence+1, because the snapshot supersedes those events.
type CursorGap struct {
	Requested        int64
	NextSequence     int64
	SnapshotID       string
	SnapshotSequence int64
}
