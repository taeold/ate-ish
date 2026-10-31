// Package store is the handful of operations ate-ish's control plane
// (internal/ingress, internal/atelet, internal/egress) needs on the
// object store: a JSON record with a generation (get/put, put's gen 0
// meaning "must not exist"), delete, prefix list, and the blob
// upload/download/copy snapshots and tags use. bucket.go (GCS) and
// dirstore.go (a local directory) both implement it.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/taeold/ate-ish/internal/envutil"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrConflict    = errors.New("generation changed")
	ErrRateLimited = errors.New("rate limited")
)

// Store is the interface every backend implements.
type Store interface {
	// Get reads a JSON record into v and returns its generation.
	Get(ctx context.Context, name string, v any) (int64, error)
	// Put writes v as JSON if the object's generation is still gen (0
	// means "create, must not already exist"; see the callers' own use of
	// -1 through UploadFile for "unconditional"), returning the new
	// generation.
	Put(ctx context.Context, name string, v any, gen int64) (int64, error)
	Delete(ctx context.Context, name string) error
	// List returns the names of every object under prefix.
	List(ctx context.Context, prefix string) ([]string, error)
	UploadFile(ctx context.Context, name, file string) error
	DownloadFile(ctx context.Context, name, file string) error
	// Copy duplicates src to dst unconditionally (used for a tag's
	// snapshot and for starting a new actor from a tag).
	Copy(ctx context.Context, src, dst string) error
}

// DefaultDir is the local-directory store's default path when neither
// ATE_STORE nor ATE_BUCKET is set: $XDG_STATE_HOME/ate-ish/store, or
// ~/.local/state/ate-ish/store.
func DefaultDir() string {
	base := envutil.Get("XDG_STATE_HOME", "")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/root"
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ate-ish", "store")
}

// New picks the store backend from the environment: ATE_STORE=dir:<path>
// for the local-directory implementation, or ATE_BUCKET=<name> for GCS
// (the only cloud backend today). With neither set, it falls back to the
// local-directory implementation at DefaultDir(), so a bare `ate-ish`
// invocation always has somewhere to keep its state.
func New(ctx context.Context) (Store, error) {
	if spec := envutil.Get("ATE_STORE", ""); spec != "" {
		path, ok := strings.CutPrefix(spec, "dir:")
		if !ok {
			return nil, fmt.Errorf("ATE_STORE=%q: only the dir:<path> form is supported", spec)
		}
		return newDirStore(path)
	}
	if name := envutil.Get("ATE_BUCKET", ""); name != "" {
		return newBucket(ctx, name)
	}
	return newDirStore(DefaultDir())
}
