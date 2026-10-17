package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type Filesystem struct {
	root string
}

func NewFilesystem(dir string) (*Filesystem, error) {
	if dir == "" {
		return nil, errors.New("storage: filesystem.dir is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Filesystem{root: dir}, nil
}

func (f *Filesystem) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if clean == "/" || strings.Contains(key, "\x00") {
		return "", fmt.Errorf("storage: invalid key %q", key)
	}
	return filepath.Join(f.root, filepath.FromSlash(clean)), nil
}

func (f *Filesystem) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, ErrNotExist
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

func (f *Filesystem) Stat(_ context.Context, key string) (Object, error) {
	p, err := f.path(key)
	if err != nil {
		return Object{}, err
	}
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Object{}, ErrNotExist
	}
	if err != nil {
		return Object{}, err
	}
	if info.IsDir() {
		return Object{}, ErrNotExist
	}
	return Object{Key: key, Size: info.Size(), Modified: info.ModTime()}, nil
}

func (f *Filesystem) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (f *Filesystem) Delete(_ context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(p)
	if dir != f.root {
		_ = os.Remove(dir)
	}
	return nil
}

func (f *Filesystem) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != f.root {
				return filepath.SkipDir
			}
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != f.root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(f.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, Object{Key: key, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	return out, err
}
