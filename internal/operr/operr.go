// Package operr provides marker errors that classify operation failures so
// HTTP handlers can map them to status codes with errors.Is instead of
// matching message strings (which silently broke whenever a message was
// reworded). The markers never appear in the error text.
package operr

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound  = errors.New("not found")     // a referenced resource does not exist -> 404
	ErrConflict  = errors.New("conflict")      // the resource already exists / is in use -> 409
	ErrInvalid   = errors.New("invalid input") // the request cannot be satisfied as specified -> 400
	ErrForbidden = errors.New("forbidden")     // the action is refused by policy -> 403
)

// NotFoundf, Conflictf, Invalidf and Forbiddenf format an error exactly like
// fmt.Errorf (including %w wrapping) and tag it with the corresponding marker.
// The tag survives further fmt.Errorf("...: %w", err) wrapping.
func NotFoundf(format string, args ...any) error { return mark(ErrNotFound, format, args...) }

func Conflictf(format string, args ...any) error { return mark(ErrConflict, format, args...) }

func Invalidf(format string, args ...any) error { return mark(ErrInvalid, format, args...) }

func Forbiddenf(format string, args ...any) error { return mark(ErrForbidden, format, args...) }

func mark(marker error, format string, args ...any) error {
	return &markedError{err: fmt.Errorf(format, args...), marker: marker}
}

type markedError struct {
	err    error
	marker error
}

func (e *markedError) Error() string { return e.err.Error() }

func (e *markedError) Unwrap() []error { return []error{e.err, e.marker} }
