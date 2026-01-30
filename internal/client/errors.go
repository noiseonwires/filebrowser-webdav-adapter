package client

import "errors"

var (
	// ErrUnauthorized is returned when authentication fails.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrNotFound is returned when the requested resource doesn't exist.
	ErrNotFound = errors.New("not found")

	// ErrForbidden is returned when the user lacks permission.
	ErrForbidden = errors.New("forbidden")

	// ErrConflict is returned when there's a conflict (e.g., file exists).
	ErrConflict = errors.New("conflict")

	// ErrBadRequest is returned for invalid requests.
	ErrBadRequest = errors.New("bad request")
)
