//go:build live

package credentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/skillum-ai/orpheus/internal/agentbox"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/harness/codex"
	"github.com/skillum-ai/orpheus/internal/session"
)

func TestLiveAccountFiles(t *testing.T) {
	if os.Getenv("AGENTBOX_API_KEY") == "" || os.Getenv("TEST_S3_ENDPOINT") == "" {
		t.Fatal("AGENTBOX_API_KEY and TEST_S3_ENDPOINT are required")
	}
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("orpheus-test", "orpheus-test-password", "")}, func(o *s3.Options) { o.BaseEndpoint = &endpoint; o.UsePathStyle = true })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	bucket := "orpheus-" + uuid.NewString()
	key := "auth.json"
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = client.DeleteObject(cleanup, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key})
		_, _ = client.DeleteBucket(cleanup, &s3.DeleteBucketInput{Bucket: &bucket})
	})
	platform, err := agentbox.New()
	if err != nil {
		t.Fatal(err)
	}
	box, err := platform.Create(ctx, "codex", 180*time.Second, map[string]string{"purpose": "orpheus-go-auth-test"})
	if err != nil {
		t.Fatal(err)
	}
	sdkClient, err := sdk.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := sdkClient.Sandboxes.Kill(cleanup, box.ID()); err != nil {
			t.Error("sandbox cleanup failed")
		}
	})
	rawHome, err := box.Run(ctx, "mktemp -d")
	if err != nil {
		t.Fatal(err)
	}
	home := strings.TrimSpace(string(rawHome))
	fixture := func(refresh string) []byte {
		claims, _ := json.Marshal(map[string]any{"email": "fixture@example.test", "sub": "fixture", "exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "fixture-account", "chatgpt_plan_type": "plus"}})
		raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]string{"access_token": "fixture-access", "refresh_token": refresh, "id_token": "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})
		return raw
	}
	source := &s3Store{client: client, bucket: bucket, key: key}
	if err := source.Put(ctx, fixture("first")); err != nil {
		t.Fatal(err)
	}
	account := NewWithStore(box, home, source)
	defer func() { _ = account.Close() }()
	if err := account.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	mode, err := box.Run(ctx, "stat -c '%U:%a' "+harness.Quote(home+"/auth.json"))
	if err != nil || strings.TrimSpace(string(mode)) != "user:600" {
		t.Fatal("auth file owner or mode", err)
	}
	driver := codex.New(box, 30*time.Second, 524288)
	defer func() { _ = driver.Close() }()
	creds := session.Credentials{Mode: "account"}
	env, err := driver.Prepare(ctx, home, creds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Launch(ctx, env, home); err != nil {
		t.Fatal(err)
	}
	if err := driver.Initialize(ctx, creds, true); err != nil {
		t.Fatal(err)
	}
	if err := account.Watch(ctx); err != nil {
		t.Fatal(err)
	}
	account.Sync(ctx, true)
	rotated := fixture("rotated")
	if err := box.Write(ctx, home+"/auth.json", rotated); err != nil {
		t.Fatal(err)
	}
	for !account.dirty.Load() {
		select {
		case <-ctx.Done():
			t.Fatal("watch missed auth.json update")
		case <-time.After(20 * time.Millisecond):
		}
	}
	account.Sync(ctx, false)
	saved, err := source.Get(ctx)
	if err != nil || string(saved) != string(rotated) {
		t.Fatal("rotated credentials not persisted", err)
	}
	// This validates native auth-file recognition and rotation transport, not a
	// real OAuth refresh with production account tokens.
}
