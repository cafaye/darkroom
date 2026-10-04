package variants

import (
	"errors"
	"strconv"
)

// The failure vocabulary this package needs, and no more.
//
// `variants.rs` returned `crate::error::Error`, and the two constructors it used
// were `Error::invalid_fields` and `Error::internal`. This file is the Go version
// of the subset of `src/error.rs` those two call, carried here rather than
// invented as a shared package, because:
//
//   - The HTTP translation — `Error::to_problem(path, request_id)`, which turns
//     this into an RFC 9457 body — belongs to the packet that owns
//     `internal/httpapi`, and nothing in this file needs it. A `to_problem` here
//     would be a method with no caller and a shape the HTTP packet should get to
//     choose.
//   - The one property this package's tests assert is the STATUS: a decode
//     failure is a 422 and an encoder failure is a 500. That is the distinction
//     the packet is about, so it is the distinction carried.
//
// If a later packet introduces a platform-wide error type, these two
// constructors should move into it and every number here should stay. Nothing
// outside this file may construct one of these directly: `undecodable` is the
// only path a decode failure takes, and that is what keeps "a decode failure is a
// 422" a property of the code rather than a property of the callers.

// StatusUnprocessable is the status a decode failure carries.
//
// 422 rather than 500 because the bytes were verified by checksum before they got
// here, so they are exactly what the client sent, and a client that declared
// `image/png` and sent a PDF has made a mistake it can see and fix.
const StatusUnprocessable = 422

// StatusInternal is the status a failure that is ours carries.
//
// It is a named constant for the same reason [JPEGQuality] is: the number is a
// decision somebody made, so it is written down once and asserted rather than
// typed at a call site.
const StatusInternal = 500

// FieldError names the one thing the caller has to change, and how.
//
// A problem with no field name is a problem with nothing actionable in it, so
// `Fields` is not optional in the way the Rust `Vec<FieldError>` is: an internal
// failure simply carries none.
type FieldError struct {
	Field string
	Code  string
}

// Error is a failure this package returned deliberately.
//
// It is not the platform's error type and does not claim to be; see above. It is
// returned as `error` so callers use `errors.As` and are not coupled to this
// struct, which is what lets it move.
type Error struct {
	Status int
	Detail string
	Fields []FieldError
}

func (e *Error) Error() string {
	return strconv.Itoa(e.Status) + ": " + e.Detail
}

// invalidFields is a 422 naming what the caller must change.
func invalidFields(detail string, fields ...FieldError) *Error {
	return &Error{Status: StatusUnprocessable, Detail: detail, Fields: fields}
}

// internal is a 500: a failure in this repository, not in the request.
func internal(detail string) *Error {
	return &Error{Status: StatusInternal, Detail: detail}
}

// asError extracts the deliberate failure from `err`, or reports that there was
// none.
func asError(err error) (*Error, bool) {
	var target *Error
	ok := errors.As(err, &target)
	return target, ok
}
