// Package apierr is the sentinel errors and the one HTTP-reply helper
// shared by internal/ingress's admin API and internal/atelet's worker
// API, so the two can map the same error to the same status code without
// importing each other.
package apierr

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/taeold/ate-ish/internal/store"
)

var (
	// ErrNoRoom: no live worker has a free slot for the actor (ingress's
	// own placement loop gives up after retrying).
	ErrNoRoom = errors.New("no worker has room for the actor")
	// ErrExists: a create call named something that already exists.
	ErrExists = errors.New("already exists")
	// ErrFull: a worker has no free slot (reconstructed from a 503 by
	// ingress's own call to a worker, or returned directly within a
	// worker process).
	ErrFull = errors.New("worker has no free slot")
	// ErrStopping: the actor is suspending on the worker that has it.
	ErrStopping = errors.New("actor is stopping on this worker")
	// ErrNotHosted: the actor is not running on this worker.
	ErrNotHosted = errors.New("actor is not running on this worker")
)

// Reply writes v as JSON on success, or maps err to the right HTTP status
// otherwise.
func Reply(rw http.ResponseWriter, v any, err error) {
	switch {
	case errors.Is(err, ErrFull), errors.Is(err, ErrStopping), errors.Is(err, ErrNoRoom):
		http.Error(rw, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, ErrNotHosted), errors.Is(err, store.ErrConflict), errors.Is(err, ErrExists):
		http.Error(rw, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrNotFound):
		http.Error(rw, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(rw, err.Error(), http.StatusInternalServerError)
	default:
		rw.Header().Set("content-type", "application/json")
		json.NewEncoder(rw).Encode(v)
	}
}

// ExistsErr turns a store.ErrConflict from a create call's conditional
// write (the record already existed) into ErrExists.
func ExistsErr(err error) error {
	if errors.Is(err, store.ErrConflict) {
		return ErrExists
	}
	return err
}
