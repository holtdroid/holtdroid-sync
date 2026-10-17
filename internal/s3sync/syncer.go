package s3sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/kriakiku/holtdroid-sync/internal/apk"
	"github.com/kriakiku/holtdroid-sync/internal/layout"
	"github.com/kriakiku/holtdroid-sync/internal/rules"
	"github.com/kriakiku/holtdroid-sync/internal/storage"
)

const maxMetaSize = 1 << 20

type Source struct {
	Client *s3.Client
	Bucket string
	Prefix string
	Region string
	AWS    bool
	Rules  rules.Rules
}

type Syncer struct {
	src   Source
	store storage.Store
	prune bool
	log   *slog.Logger
}

func New(src Source, store storage.Store, prune bool) *Syncer {
	src.Prefix = strings.Trim(src.Prefix, "/")
	if src.Prefix != "" {
		src.Prefix += "/"
	}
	return &Syncer{src: src, store: store, prune: prune, log: slog.Default().With("component", "syncer")}
}

type object struct {
	key      string
	size     int64
	modified time.Time
}

func (s *Syncer) Sync(ctx context.Context) (layout.Stats, error) {
	var st layout.Stats
	all, err := s.list(ctx)
	if err != nil {
		return st, err
	}
	want := map[string]object{}
	for rel, o := range all {
		if layout.IsAPK(rel) && s.src.Rules.Match(rel) {
			want[rel] = o
		}
	}

	objs, err := s.store.List(ctx, "")
	if err != nil {
		return st, fmt.Errorf("list target: %w", err)
	}
	have := map[string]storage.Object{}
	for _, o := range objs {
		have[o.Key] = o
	}

	var errs []error
	for rel, o := range want {
		if t, ok := have[rel]; !layout.Outdated(t, ok, o.size, o.modified) {
			st.Skipped++
			continue
		}
		b, err := s.copy(ctx, rel, o, all)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel, err))
			continue
		}
		st.Copied++
		st.Builds = append(st.Builds, b)
		s.log.Info("copied", "key", rel, "size", o.size)
	}

	if s.prune {
		for key := range have {
			if _, keep := want[key]; keep || !layout.IsAPK(key) {
				continue
			}
			if err := layout.Remove(ctx, s.store, key); err != nil {
				errs = append(errs, fmt.Errorf("prune: %w", err))
			}
			st.Pruned++
			s.log.Info("pruned", "key", key)
		}
	}
	return st, errors.Join(errs...)
}

func (s *Syncer) list(ctx context.Context) (map[string]object, error) {
	out := map[string]object{}
	pages := s3.NewListObjectsV2Paginator(s.src.Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.src.Bucket),
		Prefix: aws.String(s.src.Prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", s.src.Bucket, s.src.Prefix, err)
		}
		for _, o := range page.Contents {
			rel := strings.TrimPrefix(aws.ToString(o.Key), s.src.Prefix)
			if rel == "" || strings.HasSuffix(rel, "/") || layout.Hidden(rel) {
				continue
			}
			out[rel] = object{key: aws.ToString(o.Key), size: aws.ToInt64(o.Size), modified: aws.ToTime(o.LastModified)}
		}
	}
	return out, nil
}

func (s *Syncer) copy(ctx context.Context, rel string, o object, all map[string]object) (layout.Build, error) {
	body, err := s.get(ctx, o.key)
	if err != nil {
		return layout.Build{}, err
	}
	staged, err := layout.Stage(body, o.size)
	_ = body.Close()
	if err != nil {
		return layout.Build{}, err
	}
	defer staged.Close()

	if side, ok := all[rel+layout.IconSuffix]; ok {
		if raw, err := s.read(ctx, side.key, apk.MaxIconBytes); err == nil {
			if normalized, err := apk.NormalizeIcon(raw); err == nil {
				staged.Icon = normalized
			} else {
				s.log.Warn("ignoring icon sidecar", "key", rel, "err", err)
			}
		}
	}
	meta, err := s.meta(ctx, rel, o, all)
	if err != nil {
		return layout.Build{}, err
	}
	if err := staged.Publish(ctx, s.store, rel, meta); err != nil {
		return layout.Build{}, err
	}
	return layout.Build{Key: rel, Size: staged.Size, Modified: o.modified, Link: meta.Link, App: staged.App}, nil
}

func (s *Syncer) meta(ctx context.Context, rel string, o object, all map[string]object) (layout.Meta, error) {
	m := layout.Meta{UploadedAt: o.modified.UTC(), Origin: "s3", Link: s.consoleLink(o.key)}
	if side, ok := all[rel+layout.MetaSuffix]; ok {
		raw, err := s.read(ctx, side.key, maxMetaSize)
		if err != nil {
			return m, fmt.Errorf("read meta sidecar: %w", err)
		}
		var published layout.Meta
		if err := json.Unmarshal(raw, &published); err != nil {
			s.log.Warn("ignoring an invalid meta sidecar", "key", rel, "err", err)
		} else {
			m = merge(published, m)
		}
	}
	return m, nil
}

func merge(published, defaults layout.Meta) layout.Meta {
	if published.UploadedAt.IsZero() {
		published.UploadedAt = defaults.UploadedAt
	}
	if published.Origin == "" {
		published.Origin = defaults.Origin
	}
	if published.Link == "" {
		published.Link = defaults.Link
	}
	return published
}

func (s *Syncer) consoleLink(key string) string {
	if !s.src.AWS || s.src.Region == "" {
		return ""
	}
	q := url.Values{"region": {s.src.Region}, "prefix": {key}}
	return "https://" + s.src.Region + ".console.aws.amazon.com/s3/object/" + url.PathEscape(s.src.Bucket) + "?" + q.Encode()
}

func (s *Syncer) get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.src.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.src.Bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("get s3://%s/%s: %w", s.src.Bucket, key, err)
	}
	return out.Body, nil
}

func (s *Syncer) read(ctx context.Context, key string, limit int64) ([]byte, error) {
	body, err := s.get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", key, limit)
	}
	return raw, nil
}
