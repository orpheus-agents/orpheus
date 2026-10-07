//go:build integration

package credentials

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

func TestS3RoundTrip(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("TEST_S3_ENDPOINT is required for integration tests")
	}
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("orpheus-test", "orpheus-test-password", "")}, func(o *s3.Options) { o.BaseEndpoint = &endpoint; o.UsePathStyle = true })
	bucket := "orpheus-" + uuid.NewString()
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatal(err)
	}
	key := "auth.json"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key})
		_, _ = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket})
	})
	source := &s3Store{client: client, bucket: bucket, key: key}
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &bucket, Key: &key, Body: bytes.NewReader([]byte(authA))}); err != nil {
		t.Fatal(err)
	}
	box := &fakeBox{}
	account := NewWithStore(box, "/home", source)
	defer func() { _ = account.Close() }()
	if err := account.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	staleBox := &fakeBox{}
	stale := NewWithStore(staleBox, "/home", source)
	defer func() { _ = stale.Close() }()
	if err := stale.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	box.raw = authB
	account.Sync(t.Context(), true)
	staleBox.raw = authC
	stale.Sync(t.Context(), true)
	blob, err := source.Get(t.Context())
	if err != nil || string(blob.Raw) != authB {
		t.Fatal("S3 rotation failed", err)
	}
}
