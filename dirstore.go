package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// dirStore is ATE_STORE=dir:<path>'s local-directory store: GCS's own
// get/put/delete/list/copy semantics with no cloud account, files under
// <path>/objects, generation in a sidecar .ver, CAS via mutex plus flock.
type dirStore struct {
	root string
	mu   sync.Mutex
}

func newDirStore(path string) (*dirStore, error) {
	if err := os.MkdirAll(filepath.Join(path, "objects"), 0o755); err != nil {
		return nil, err
	}
	return &dirStore{root: path}, nil
}
func (d *dirStore) objPath(name string) string { return filepath.Join(d.root, "objects", name) }

func (d *dirStore) version(name string) (int64, error) {
	data, err := os.ReadFile(d.objPath(name) + ".ver")
	if os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	} else if err := os.WriteFile(path+".tmp", data, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
func (d *dirStore) readObj(name string) ([]byte, error) {
	d.mu.Lock()
	data, err := os.ReadFile(d.objPath(name))
	d.mu.Unlock()
	if os.IsNotExist(err) {
		return nil, errNotFound
	}
	return data, err
}
func (d *dirStore) writeCAS(name string, data []byte, gen int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(d.objPath(name)), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(d.objPath(name)+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	cur, err := d.version(name)
	if err != nil {
		return 0, err
	} else if gen >= 0 && gen != cur {
		return 0, errConflict
	}
	next := cur + 1
	if err := atomicWrite(d.objPath(name), data); err != nil {
		return 0, err
	}
	return next, atomicWrite(d.objPath(name)+".ver", []byte(strconv.FormatInt(next, 10)))
}
func (d *dirStore) get(_ context.Context, name string, v any) (int64, error) {
	data, err := d.readObj(name)
	if err != nil {
		return 0, err
	}
	gen, err := d.version(name)
	if err != nil {
		return 0, err
	}
	return gen, json.Unmarshal(data, v)
}
func (d *dirStore) put(_ context.Context, name string, v any, gen int64) (int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return d.writeCAS(name, data, gen)
}
func (d *dirStore) delete(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.Remove(d.objPath(name)); os.IsNotExist(err) {
		return errNotFound
	} else if err != nil {
		return err
	}
	os.Remove(d.objPath(name) + ".ver")
	return nil
}
func (d *dirStore) list(_ context.Context, prefix string) ([]string, error) {
	var names []string
	root := filepath.Join(d.root, "objects")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		if info.IsDir() || strings.HasSuffix(path, ".ver") || strings.HasSuffix(path, ".lock") || strings.HasSuffix(path, ".tmp") {
			return nil
		}
		if name := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator))); strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
		return nil
	})
	return names, err
}
func (d *dirStore) uploadFile(_ context.Context, name, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	_, err = d.writeCAS(name, data, -1)
	return err
}
func (d *dirStore) downloadFile(_ context.Context, name, file string) error {
	data, err := d.readObj(name)
	if err != nil {
		return err
	}
	return os.WriteFile(file, data, 0o644)
}
func (d *dirStore) copy(_ context.Context, src, dst string) error {
	data, err := d.readObj(src)
	if err != nil {
		return err
	}
	_, err = d.writeCAS(dst, data, -1)
	return err
}
