// Package snapshot stores workspace snapshots in S3-API object storage (docs/architecture.md D7). The server only presigns;
// the bytes flow directly between the sandbox (sandbox-snap) and the object store.
package snapshot

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Store struct {
	c      *minio.Client
	bucket string
	ttl    time.Duration
}

func New(endpoint, accessKey, secretKey, bucket string, secure bool) (*Store, error) {
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: secure,
		Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
	if err != nil {
		return nil, err
	}
	return &Store{c: c, bucket: bucket, ttl: 15 * time.Minute}, nil
}

func Prefix(sandboxID string, gen int64) string { return fmt.Sprintf("ws/%s/g%d/", sandboxID, gen) }

// AutosavePrefix creates a distinct target for each periodic archive so it
// never overwrites the last committed snapshot before the pointer transition.
func AutosavePrefix(sandboxID string, gen int64, revision int64) string {
	return fmt.Sprintf("%sauto/%020d/", Prefix(sandboxID, gen), revision)
}

func chunkKey(prefix string, i int) string { return fmt.Sprintf("%spart.%05d", prefix, i) }

// PutURLs returns n presigned PUT URLs for chunks under prefix.
func (s *Store) PutURLs(ctx context.Context, prefix string, n int) ([]string, error) {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		u, err := s.c.PresignedPutObject(ctx, s.bucket, chunkKey(prefix, i), s.ttl)
		if err != nil {
			return nil, err
		}
		out[i] = u.String()
	}
	return out, nil
}

// GetURLs lists the chunks under prefix and returns presigned GET URLs in order.
func (s *Store) GetURLs(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, o.Err
		}
		keys = append(keys, o.Key)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		u, err := s.c.PresignedGetObject(ctx, s.bucket, k, s.ttl, url.Values{})
		if err != nil {
			return nil, err
		}
		out[i] = u.String()
	}
	return out, nil
}

// Delete removes all chunks under prefix (old generations / terminated sandboxes).
func (s *Store) Delete(ctx context.Context, prefix string) error {
	objs := s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	for e := range s.c.RemoveObjects(ctx, s.bucket, objs, minio.RemoveObjectsOptions{}) {
		if e.Err != nil {
			return e.Err
		}
	}
	return nil
}
