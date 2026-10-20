package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/holtdroid/holtdroid-sync/internal/secrets"
)

type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

func NewS3(ctx context.Context, cfg S3Config, reg *secrets.Registry) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("storage: s3.endpoint and s3.bucket are required")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage: s3.endpoint must be a URL like https://s3.example.com: %q", cfg.Endpoint)
	}
	var creds *credentials.Credentials
	if cfg.AccessKey.IsZero() {
		creds = credentials.NewIAM("")
	} else {
		ak, err := reg.Resolve(ctx, cfg.AccessKey)
		if err != nil {
			return nil, err
		}
		sk, err := reg.Resolve(ctx, cfg.SecretKey)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewStaticV4(ak, sk, "")
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        creds,
		Secure:       u.Scheme == "https",
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, err
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3{client: client, bucket: cfg.Bucket, prefix: prefix}, nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.prefix+key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, mapS3Err(err)
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, 0, mapS3Err(err)
	}
	return obj, info.Size, nil
}

func (s *S3) Stat(ctx context.Context, key string) (Object, error) {
	info, err := s.client.StatObject(ctx, s.bucket, s.prefix+key, minio.StatObjectOptions{})
	if err != nil {
		return Object{}, mapS3Err(err)
	}
	return Object{Key: key, Size: info.Size, Modified: info.LastModified, ETag: info.ETag}, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.prefix+key, r, size, minio.PutObjectOptions{})
	return err
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return mapS3Err(s.client.RemoveObject(ctx, s.bucket, s.prefix+key, minio.RemoveObjectOptions{}))
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix + prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		out = append(out, Object{Key: strings.TrimPrefix(obj.Key, s.prefix), Size: obj.Size, Modified: obj.LastModified, ETag: obj.ETag})
	}
	return out, nil
}

func mapS3Err(err error) error {
	if err == nil {
		return nil
	}
	if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
		return ErrNotExist
	}
	return err
}
