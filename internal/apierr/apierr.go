// Package apierr maps domain and storage errors onto protocol errors.
//
// Storage and domain errors are Go errors, not protocol errors. Translating them
// at the API boundary in one place keeps the mapping from drifting between the
// Control API, the node link, and the plugin link.
package apierr

import (
	"errors"

	"github.com/thupham/hive/internal/storage"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// From maps an error onto a protocol error.
//
// A protocol error is passed through unchanged. A missing record becomes
// not-found. Anything else is reported as unavailable, because the caller can
// retry rather than being told the request was malformed.
func From(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNoChange):
		return v1.Retryable("no change was required")
	}

	var protoErr *v1.Error
	if errors.As(err, &protoErr) {
		return protoErr
	}
	if errors.Is(err, storage.ErrNotFound) {
		return v1.NotFound("%s", err.Error())
	}
	return v1.Unavailable("%s", err.Error())
}

// ErrNoChange reports that an operation decided nothing needed to change.
//
// A read-modify-write helper returns it to roll the transaction back rather than
// rewrite a row with identical values.
var ErrNoChange = errors.New("no change was required")
