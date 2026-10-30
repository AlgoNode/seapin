// Package store reads and writes cached objects in the region's S3 bucket
// (SeaweedFS).
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"seapin/internal/config"
)

// ErrNotFound means the bucket holds no object under the key.
var ErrNotFound = errors.New("object not found")

// Info is what serving needs to know about a stored object.
type Info struct {
	Size        int64
	ContentType string
}

type Store struct {
	c      *minio.Client
	bucket string
}

func New(cfg config.S3) (*Store, error) {
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return &Store{c: c, bucket: cfg.Bucket}, nil
}

func (s *Store) Stat(ctx context.Context, key string) (Info, error) {
	oi, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return Info{}, ErrNotFound
		}
		return Info{}, err
	}
	return Info{Size: oi.Size, ContentType: oi.ContentType}, nil
}

// GetRange streams the object from byte off to its end. The request is made
// before it returns, so a missing object is reported here, not on Read.
func (s *Store) GetRange(ctx context.Context, key string, off int64) (io.ReadCloser, error) {
	var opts minio.GetObjectOptions
	if off > 0 {
		if err := opts.SetRange(off, 0); err != nil {
			return nil, err
		}
	}
	body, _, _, err := minio.Core{Client: s.c}.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return body, nil
}

// Put streams r into the bucket; size may be -1 when unknown. Large objects go
// up in 8 MiB parts one at a time, so many concurrent uploads stay cheap in
// memory.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.c.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    8 << 20,
		NumThreads:  1,
	})
	return err
}

// Ping checks that the bucket is reachable and exists.
func (s *Store) Ping(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("bucket %q does not exist", s.bucket)
	}
	return nil
}

// EnsureBucket creates the bucket when it is missing. It needs an identity
// with Admin rights on the bucket (the ingest one).
func (s *Store) EnsureBucket(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil || ok {
		return err
	}
	return s.c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}

func isNotFound(err error) bool {
	r := minio.ToErrorResponse(err)
	return r.Code == "NoSuchKey" || (r.StatusCode == http.StatusNotFound && r.Code != "NoSuchBucket")
}
