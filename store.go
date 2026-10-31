package main

import (
	"context"
	"fmt"
	"strings"
)

// store is the handful of operations ate-ish's control plane (ingress.go,
// worker.go, egress.go, actors.go) needs on the object store: a JSON
// record with a generation (get/put, put's gen 0 meaning "must not
// exist"), delete, prefix list, and the blob upload/download/copy
// snapshots and tags use. bucket.go (GCS) and dirstore.go (a local
// directory) both implement it unchanged in behaviour from before this
// interface existed.
type store interface {
	// get reads a JSON record into v and returns its generation.
	get(ctx context.Context, name string, v any) (int64, error)
	// put writes v as JSON if the object's generation is still gen (0
	// means "create, must not already exist"; see the callers' own use of
	// -1 through uploadFile for "unconditional"), returning the new
	// generation.
	put(ctx context.Context, name string, v any, gen int64) (int64, error)
	delete(ctx context.Context, name string) error
	// list returns the names of every object under prefix.
	list(ctx context.Context, prefix string) ([]string, error)
	uploadFile(ctx context.Context, name, file string) error
	downloadFile(ctx context.Context, name, file string) error
	// copy duplicates src to dst unconditionally (used for a tag's
	// snapshot and for starting a new actor from a tag).
	copy(ctx context.Context, src, dst string) error
}

// newStore picks the store backend from the environment: ATE_STORE=dir:<path>
// for the local-directory implementation ate-ish dev uses, or ATE_BUCKET=<name>
// for GCS (today's only cloud backend). Exactly one of the two must be set.
func newStore(ctx context.Context) (store, error) {
	if spec := env("ATE_STORE", ""); spec != "" {
		path, ok := strings.CutPrefix(spec, "dir:")
		if !ok {
			return nil, fmt.Errorf("ATE_STORE=%q: only the dir:<path> form is supported", spec)
		}
		return newDirStore(path)
	}
	name := env("ATE_BUCKET", "")
	if name == "" {
		return nil, fmt.Errorf("set ATE_BUCKET=<gcs bucket> or ATE_STORE=dir:<path>")
	}
	return newBucket(ctx, name)
}
