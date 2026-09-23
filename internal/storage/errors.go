package storage

import "errors"

// ErrNotFound reports that a requested record does not exist.
var ErrNotFound = errors.New("storage: not found")
