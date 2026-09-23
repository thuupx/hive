package v1

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Hive domain error codes, in the JSON-RPC server error range.
//
// These are protocol-level conditions, not implementation details. A
// consumer may rely on the code to decide whether to retry, rehydrate, or
// report.
const (
	// CodeUnauthorized means the caller is not authorized for the target
	// resource. Authorization is a Hive domain concern and is evaluated
	// before a mutating operation is accepted.
	CodeUnauthorized = -32000

	// CodeNotFound means the target resource does not exist or is not
	// visible to the caller.
	CodeNotFound = -32001

	// CodeConflict means the operation conflicts with current domain
	// state, such as an invalid state transition or a command id already
	// bound to a different operation.
	CodeConflict = -32002

	// CodeCursorExpired means the requested event sequence has been
	// pruned. The response carries the latest snapshot boundary and the
	// next valid sequence.
	CodeCursorExpired = -32003

	// CodeAmbiguous means the outcome cannot be established safely. The
	// caller must not assume success or failure.
	CodeAmbiguous = -32004

	// CodeUnavailable means the operation cannot be served now because
	// required state is incomplete or a dependency is unreachable.
	CodeUnavailable = -32005

	// CodeRetryable means the operation is still in progress or was
	// interrupted before completion and may be retried with the same
	// command id.
	CodeRetryable = -32006

	// CodeUnsupported means the method or capability is known but not
	// supported by this peer. This is the explicit result required when an
	// agent protocol exposes a method Hive understands but cannot serve.
	CodeUnsupported = -32007
)

// ErrPeerClosed reports that a protocol connection is no longer usable.
var ErrPeerClosed = errors.New("hive: connection closed")

// Error is a JSON-RPC error object. It also implements the Go error
// interface so protocol errors can travel as ordinary Go errors.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("hive protocol error %d: %s", e.Code, e.Message)
}

// WithData attaches structured data to the error and returns it.
func (e *Error) WithData(v any) *Error {
	if e == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return e
	}
	e.Data = b
	return e
}

// NewErrorf builds an error with the given code and formatted message.
func NewErrorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func InvalidRequest(format string, args ...any) *Error {
	return NewErrorf(CodeInvalidRequest, format, args...)
}

func InvalidParams(format string, args ...any) *Error {
	return NewErrorf(CodeInvalidParams, format, args...)
}

func Internal(format string, args ...any) *Error {
	return NewErrorf(CodeInternalError, format, args...)
}

func MethodNotFound(method string) *Error {
	return NewErrorf(CodeMethodNotFound, "method not found: %s", method)
}

func Unauthorized(format string, args ...any) *Error {
	return NewErrorf(CodeUnauthorized, format, args...)
}

func NotFound(format string, args ...any) *Error {
	return NewErrorf(CodeNotFound, format, args...)
}

func Conflict(format string, args ...any) *Error {
	return NewErrorf(CodeConflict, format, args...)
}

func CursorExpired(format string, args ...any) *Error {
	return NewErrorf(CodeCursorExpired, format, args...)
}

func Ambiguous(format string, args ...any) *Error {
	return NewErrorf(CodeAmbiguous, format, args...)
}

func Unavailable(format string, args ...any) *Error {
	return NewErrorf(CodeUnavailable, format, args...)
}

func Retryable(format string, args ...any) *Error {
	return NewErrorf(CodeRetryable, format, args...)
}

func Unsupported(format string, args ...any) *Error {
	return NewErrorf(CodeUnsupported, format, args...)
}

// AsError extracts a protocol error from err. When err is not a protocol
// error it is wrapped as an internal error so a caller always has a code
// to return.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return NewErrorf(CodeInternalError, "%s", err.Error())
}
