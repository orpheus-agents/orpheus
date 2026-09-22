// Package credentials synchronizes account auth files between S3 and a sandbox.
// Credential contents never enter PostgreSQL, errors, or logs.
package credentials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
)

const MaxAuthBytes = 1 << 20

func Validate(raw []byte) error {
	if len(raw) > MaxAuthBytes {
		return errors.New("credentials file is too large")
	}
	var value struct {
		Tokens struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			ID      string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Tokens.Access == "" || value.Tokens.Refresh == "" || value.Tokens.ID == "" {
		return errors.New("invalid account credentials")
	}
	return nil
}

type BlobStore interface {
	Get(context.Context) ([]byte, error)
	Put(context.Context, []byte) error
}
type Sync interface {
	Seed(context.Context) error
	Watch(context.Context) error
	Sync(context.Context, bool)
	Close() error
}
type Account struct {
	sandbox     harness.Sandbox
	home        string
	source      BlobStore
	digest      [32]byte
	dirty       atomic.Bool
	watch       harness.Watch
	watchCancel context.CancelFunc
	watchDone   chan struct{}
}

func New(ctx context.Context, sandbox harness.Sandbox, home string, source session.Credentials) (*Account, error) {
	if source.Store == nil {
		return nil, errors.New("credential store is required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(source.Store.Region), awsconfig.WithRetryMaxAttempts(2))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = source.Store.EndpointURL
		o.UsePathStyle = source.Store.EndpointURL != nil
	})
	return NewWithStore(sandbox, home, &s3Store{client: client, bucket: source.Store.Bucket, key: source.Key}), nil
}
func NewWithStore(sandbox harness.Sandbox, home string, source BlobStore) *Account {
	a := &Account{sandbox: sandbox, home: home, source: source}
	a.dirty.Store(true)
	return a
}
func (a *Account) Seed(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := a.source.Get(ctx)
	if err == nil {
		err = Validate(raw)
	}
	if err == nil {
		err = a.sandbox.Write(ctx, a.home+"/auth.json", raw)
	}
	if err == nil {
		_, err = a.sandbox.Run(ctx, "chmod 600 "+harness.Quote(a.home+"/auth.json"))
	}
	if err != nil {
		return harness.Failure("credentials_unavailable", "Account credentials are unavailable.")
	}
	a.digest = sha256.Sum256(raw)
	return nil
}
func (a *Account) Watch(ctx context.Context) error {
	if a.watch != nil {
		select {
		case <-a.watchDone:
			a.watchCancel()
			_ = a.watch.Close()
			a.watch = nil
		default:
			return nil
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	watch, err := a.sandbox.Watch(ctx, a.home)
	if err != nil {
		cancel()
		return err
	}
	a.watch = watch
	a.watchCancel = cancel
	a.watchDone = make(chan struct{})
	a.dirty.Store(true)
	go func() {
		defer close(a.watchDone)
		defer a.dirty.Store(true)
		for {
			select {
			case <-ctx.Done():
				return
			case <-watch.Done():
				return
			case _, ok := <-watch.Events():
				if !ok {
					return
				}
				// Watch notifications may be coalesced. Any directory change
				// invalidates the digest; filtering names could miss auth.json.
				a.dirty.Store(true)
			}
		}
	}()
	return nil
}
func (a *Account) Sync(ctx context.Context, force bool) {
	if !force && !a.dirty.Load() && a.watch != nil {
		select {
		case <-a.watchDone:
		default:
			return
		}
	}
	if err := a.sync(ctx); err != nil {
		a.dirty.Store(true)
		slog.WarnContext(ctx, "Account credential upload failed")
	}
}
func (a *Account) sync(ctx context.Context) error {
	if err := a.Watch(ctx); err != nil {
		return err
	}
	a.dirty.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var raw []byte
	for attempt := range 3 {
		file, err := a.sandbox.Read(ctx, a.home+"/auth.json")
		if err != nil {
			return err
		}
		raw, err = io.ReadAll(io.LimitReader(file, MaxAuthBytes+1))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err := Validate(raw); err == nil {
			break
		} else if attempt == 2 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	digest := sha256.Sum256(raw)
	if digest == a.digest {
		return nil
	}
	if err := a.source.Put(ctx, raw); err != nil {
		return err
	}
	a.digest = digest
	return nil
}
func (a *Account) Close() error {
	if a.watch == nil {
		return nil
	}
	a.watchCancel()
	err := a.watch.Close()
	<-a.watchDone
	a.watch = nil
	return err
}

type s3Store struct {
	client      *s3.Client
	bucket, key string
}

func (s *s3Store) Get(ctx context.Context) ([]byte, error) {
	response, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	return io.ReadAll(io.LimitReader(response.Body, MaxAuthBytes+1))
}
func (s *s3Store) Put(ctx context.Context, raw []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key), Body: bytes.NewReader(raw), ContentType: aws.String("application/json")})
	return err
}
