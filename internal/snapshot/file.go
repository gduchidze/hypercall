package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// FileObjects is a durable local adapter for a private directory. All cooperating
// processes use an advisory lock. Local administrators remain trusted.
type FileObjects struct{ root string }

func NewFileObjects(root string) (*FileObjects, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: object directory must be private", ErrInvalid)
	}
	if err = os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	f := &FileObjects{root: root}
	l, err := f.lock(context.Background())
	if err != nil {
		return nil, err
	}
	defer unlock(l)
	// A writer holds the same lock through rename, so only crashed writes can
	// leave these temporary files while startup owns the lock.
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), ".upload-") {
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (f *FileObjects) lock(ctx context.Context) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l, err := os.OpenFile(filepath.Join(f.root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// Nonblocking acquisition keeps cancellation effective even behind a slow writer.
	for {
		err = unix.Flock(int(l.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			l.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			l.Close()
			return nil, ctx.Err()
		case <-timeAfterLock():
		}
	}
}
func unlock(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }

type fileReader struct {
	io.Reader
	file *os.File
}

func (r *fileReader) Close() error { return r.file.Close() }
func (f *FileObjects) get(key string) (io.ReadCloser, string, error) {
	r, err := os.Open(filepath.Join(f.root, filepath.FromSlash(key)) + ".obj")
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	var header [33]byte
	if _, err = io.ReadFull(r, header[:]); err != nil || header[32] != '\n' {
		r.Close()
		return nil, "", ErrIntegrity
	}
	gen := string(header[:32])
	decoded, e := hex.DecodeString(gen)
	if e != nil || len(decoded) != 16 {
		r.Close()
		return nil, "", ErrIntegrity
	}
	return &fileReader{Reader: r, file: r}, gen, nil
}
func (f *FileObjects) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	if !validKey(key) {
		return nil, "", ErrInvalid
	}
	l, err := f.lock(ctx)
	if err != nil {
		return nil, "", err
	}
	defer unlock(l)
	return f.get(key)
}
func (f *FileObjects) Put(ctx context.Context, key string, src io.Reader, size int64, expected string) (string, error) {
	if !validKey(key) || size < 0 || size == int64(^uint64(0)>>1) {
		return "", ErrInvalid
	}
	l, err := f.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock(l)
	old, gen, err := f.get(key)
	if old != nil {
		old.Close()
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if (errors.Is(err, ErrNotFound) && expected != "") || (err == nil && (expected == "" || gen != expected)) {
		return "", ErrPrecondition
	}
	target := filepath.Join(f.root, filepath.FromSlash(key)) + ".obj"
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".upload-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	gen = randomID()
	if _, err = io.WriteString(tmp, gen+"\n"); err != nil {
		return "", err
	}
	n, err := io.Copy(tmp, &contextReader{ctx: ctx, r: io.LimitReader(src, size+1)})
	if err != nil {
		return "", err
	}
	if n != size {
		return "", ErrIntegrity
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(tmp.Name(), target); err != nil {
		return "", err
	}
	if err = syncDirectories(filepath.Dir(target), f.root); err != nil {
		return "", err
	}
	return gen, nil
}
func (f *FileObjects) Delete(ctx context.Context, key, expected string) error {
	if !validKey(key) || expected == "" {
		return ErrInvalid
	}
	l, err := f.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock(l)
	r, gen, err := f.get(key)
	if err != nil {
		return err
	}
	r.Close()
	if gen != expected {
		return ErrPrecondition
	}
	p := filepath.Join(f.root, filepath.FromSlash(key)) + ".obj"
	if err = os.Remove(p); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (f *FileObjects) List(ctx context.Context, prefix, cursor string, limit int) ([]ObjectInfo, string, error) {
	if prefix != "" && !validKey(strings.TrimSuffix(prefix, "/")) {
		return nil, "", ErrInvalid
	}
	if limit < 1 || limit > 1000 {
		return nil, "", ErrInvalid
	}
	l, err := f.lock(ctx)
	if err != nil {
		return nil, "", err
	}
	defer unlock(l)
	var keys []string
	err = filepath.WalkDir(f.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".obj") {
			rel, e := filepath.Rel(f.root, path)
			if e != nil {
				return e
			}
			key := strings.TrimSuffix(filepath.ToSlash(rel), ".obj")
			if strings.HasPrefix(key, prefix) && key > cursor {
				keys = append(keys, key)
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Strings(keys)
	next := ""
	if len(keys) > limit {
		keys = keys[:limit]
		next = keys[len(keys)-1]
	}
	result := make([]ObjectInfo, 0, len(keys))
	for _, key := range keys {
		r, g, e := f.get(key)
		if e != nil {
			return nil, "", e
		}
		r.Close()
		result = append(result, ObjectInfo{Key: key, Generation: g})
	}
	return result, next, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Sync each new directory entry up to the durable object root as well as the
// object directory, so a successful write survives a crash after MkdirAll.
func syncDirectories(dir, root string) error {
	for {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = errors.Join(d.Sync(), d.Close())
		if err != nil {
			return err
		}
		if dir == root {
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ErrInvalid
		}
		dir = parent
	}
}
