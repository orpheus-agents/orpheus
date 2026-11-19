//go:build integration

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

// The control plane and S3 are real. The simulated native endpoint exposes
// independent seed/rotate state for each sandbox sharing the same account.
type consoleWorkerDriver struct {
	*fakeDriver
	native json.RawMessage
}

func (d *consoleWorkerDriver) ImportCredential(_ context.Context, raw json.RawMessage, _ string) error {
	d.native = bytes.Clone(raw)
	return nil
}
func (d *consoleWorkerDriver) ExportCredential(context.Context) (json.RawMessage, error) {
	return d.native, nil
}

type consoleWorkerBox struct {
	*remote
	marker []byte
}

func (b *consoleWorkerBox) Create(ctx context.Context, template string, ttl time.Duration, metadata map[string]string) (harness.Sandbox, error) {
	_, err := b.remote.Create(ctx, template, ttl, metadata)
	return b, err
}
func (b *consoleWorkerBox) Connect(ctx context.Context, id string, ttl time.Duration) (harness.Sandbox, error) {
	_, err := b.remote.Connect(ctx, id, ttl)
	return b, err
}
func (b *consoleWorkerBox) Read(ctx context.Context, path string) (io.ReadCloser, error) {
	if !strings.HasSuffix(path, "orpheus-credential-version.json") {
		return b.remote.Read(ctx, path)
	}
	if b.marker == nil {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b.marker)), nil
}
func (b *consoleWorkerBox) Write(_ context.Context, _ string, raw []byte) error {
	b.marker = bytes.Clone(raw)
	return nil
}

func TestWorkerOpenCodeConsoleRotationAcrossSandboxes(t *testing.T) {
	const initial = `{"id":"cred_worker","active":true,"integrationID":"opencode","label":"Console","value":{"type":"oauth","methodID":"device","access":"access-original","refresh":"refresh-original","expires":99999,"metadata":{"server":"https://opencode.ai/console","orgID":"org","accountID":"account"}}}`
	ctx := t.Context()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("TEST_S3_ENDPOINT required")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "orpheus-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "orpheus-test-password")
	t.Setenv("AWS_SESSION_TOKEN", "")
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("orpheus-test", "orpheus-test-password", "")}, func(o *s3.Options) { o.BaseEndpoint = &endpoint; o.UsePathStyle = true })
	bucket := "console-" + uuid.NewString()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatal(err)
	}
	var key string
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if key != "" {
			_, _ = client.DeleteObject(cleanup, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key})
		}
		_, _ = client.DeleteBucket(cleanup, &s3.DeleteBucketInput{Bucket: &bucket})
	})
	settings := config.DefaultSettings()
	settings.MaxConcurrentSessions = 4
	s := &store.Store{Pool: testutil.Database(t), Settings: settings}
	s.Deployment.CredentialStores = map[string]session.CredentialStore{"test": {Type: "s3", Bucket: bucket, Region: "us-east-1", EndpointURL: &endpoint}}
	testutil.RegisterTemplate(t, s.Pool, "opencode")
	a, err := s.CreateAccount(ctx, llm.AccountInput{Name: "Console", Harness: "opencode", AuthMode: "account", CredentialStore: new("test"), AuthFile: json.RawMessage(initial)})
	if err != nil {
		t.Fatal(err)
	}
	key = "llm-accounts/" + a.ID.String() + "/auth.json"
	p, err := s.CreateProfile(ctx, llm.ProfileInput{Name: "console-test", AccountID: a.ID, Model: "opencode-go/test"})
	if err != nil {
		t.Fatal(err)
	}
	start := func() (*Executor, *consoleWorkerDriver) {
		t.Helper()
		run, err := s.Accept(ctx, store.Admission{Key: uuid.New(), Create: &session.CreateSession{AllowMultipleRuns: true, Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: p.Name}, Sandbox: session.SandboxInput{Template: "opencode"}, Limits: session.Limits{RunTimeoutSeconds: 600}}, Messages: []session.TextMessage{{Text: "rotate"}}}})
		if err != nil {
			t.Fatal(err)
		}
		box := &consoleWorkerBox{remote: &remote{}}
		e := NewExecutor(run.SessionID, s, box)
		native := &consoleWorkerDriver{fakeDriver: &fakeDriver{r: box.remote}}
		e.NewDriver = func(harness.Sandbox, store.SessionRecord, uuid.UUID, session.Credentials) harness.Driver {
			return native
		}
		t.Cleanup(e.Disconnect)
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if e.account == nil || native.native == nil {
			t.Fatal("worker did not seed native credential")
		}
		return e, native
	}
	flush := func(e *Executor) error {
		record, run, err := e.read(ctx)
		if err != nil || run == nil {
			t.Fatalf("read worker run: %v", err)
		}
		return e.finalAuthSync(ctx, record, *run)
	}
	first, nativeA := start()
	second, nativeB := start()
	nativeA.native = []byte(strings.ReplaceAll(initial, "original", "rotated"))
	nativeB.native = []byte(strings.ReplaceAll(initial, "original", "stale"))
	if err := flush(first); err != nil {
		t.Fatal(err)
	}
	var execution *harness.ExecutionError
	if err := flush(second); !errors.As(err, &execution) || execution.Code != "credentials_unavailable" {
		t.Fatal("missing worker credential conflict", err)
	}
	saved, err := s.ReadAccountFile(ctx, a.ID)
	if err != nil || !bytes.Equal(saved.Raw, nativeA.native) {
		t.Fatal("stale worker replaced the winning credential", err)
	}
	_, third := start()
	if !bytes.Equal(third.native, nativeA.native) {
		t.Fatal("fresh sandbox received an old credential")
	}
}
