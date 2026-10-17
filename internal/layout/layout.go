// Package layout writes Holtdroid's artifact source layout: x.apk with the
// x.apk.meta.json and x.apk.icon.png sidecars (design/contracts.md
// "Artifact sources" in kriakiku/holtdroid).
package layout

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kriakiku/holtdroid-sync/internal/apk"
	"github.com/kriakiku/holtdroid-sync/internal/storage"
)

const (
	MetaSuffix = ".meta.json"
	IconSuffix = ".icon.png"
)

type Meta struct {
	Title      string            `json:"title,omitempty"`
	Note       string            `json:"note,omitempty"`
	Changelog  string            `json:"changelog,omitempty"`
	UploadedBy string            `json:"uploadedBy,omitempty"`
	UploadedAt time.Time         `json:"uploadedAt,omitzero"`
	Link       string            `json:"link,omitempty"`
	Origin     string            `json:"origin,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Owner      string            `json:"owner,omitempty"`
	Visibility string            `json:"visibility,omitempty"`
}

func (m Meta) Encode() ([]byte, error) { return json.MarshalIndent(m, "", "  ") }

type Build struct {
	Key      string
	Size     int64
	Modified time.Time
	Link     string
	App      *apk.Metadata
}

type Stats struct {
	Copied, Pruned, Skipped int
	Builds                  []Build
}

func (st Stats) Log(ctx context.Context) {
	level := slog.LevelDebug
	if st.Copied+st.Pruned > 0 {
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, "synced", "copied", st.Copied, "pruned", st.Pruned, "unchanged", st.Skipped)
}

type Staged struct {
	file *os.File
	Size int64
	App  *apk.Metadata
	Icon []byte
}

func Stage(r io.Reader, want int64) (*Staged, error) {
	tmp, err := os.CreateTemp("", "holtdroid-sync-*.apk")
	if err != nil {
		return nil, err
	}
	s := &Staged{file: tmp}
	n, err := io.Copy(tmp, r)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("download: %w", err)
	}
	if want >= 0 && n != want {
		s.Close()
		return nil, fmt.Errorf("download: got %d of %d bytes", n, want)
	}
	s.Size = n
	app, icon, err := apk.Parse(tmp.Name())
	if err != nil {
		slog.Warn("cannot read apk metadata", "err", err)
	} else {
		s.App, s.Icon = &app, icon
	}
	return s, nil
}

func (s *Staged) Close() {
	_ = s.file.Close()
	_ = os.Remove(s.file.Name())
}

func (s *Staged) Publish(ctx context.Context, store storage.Store, key string, meta Meta) error {
	raw, err := meta.Encode()
	if err != nil {
		return err
	}
	if err := PutMeta(ctx, store, key, raw); err != nil {
		return err
	}
	if s.Icon != nil {
		if err := store.Put(ctx, key+IconSuffix, bytes.NewReader(s.Icon), int64(len(s.Icon))); err != nil {
			return fmt.Errorf("write icon: %w", err)
		}
	} else if err := store.Delete(ctx, key+IconSuffix); err != nil {
		return fmt.Errorf("remove stale icon: %w", err)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := store.Put(ctx, key, s.file, s.Size); err != nil {
		return fmt.Errorf("write apk: %w", err)
	}
	return nil
}

func PutMeta(ctx context.Context, store storage.Store, key string, raw []byte) error {
	if err := store.Put(ctx, key+MetaSuffix, bytes.NewReader(raw), int64(len(raw))); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}
	return nil
}

func Remove(ctx context.Context, store storage.Store, key string) error {
	var errs []error
	for _, k := range []string{key, key + IconSuffix, key + MetaSuffix} {
		if err := store.Delete(ctx, k); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", k, err))
		}
	}
	return errors.Join(errs...)
}

func Outdated(have storage.Object, exists bool, size int64, modified time.Time) bool {
	return !exists || have.Size != size || modified.After(have.Modified)
}

func IsAPK(key string) bool { return strings.HasSuffix(strings.ToLower(key), ".apk") }

func Hidden(rel string) bool {
	for part := range strings.SplitSeq(rel, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func LaunchURL(portal, source, key string, quick bool) string {
	if portal == "" {
		return ""
	}
	q := url.Values{"source": {source}, "launch": {base64.RawURLEncoding.EncodeToString([]byte(key))}}
	if quick {
		q.Set("quick", "1")
	}
	return strings.TrimRight(portal, "/") + "/?" + q.Encode()
}

func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
