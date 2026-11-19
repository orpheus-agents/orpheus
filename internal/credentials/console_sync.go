package credentials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"time"

	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

type ConsoleClient interface {
	ImportCredential(context.Context, json.RawMessage, string) error
	ExportCredential(context.Context) (json.RawMessage, error)
}

type consoleBaseline struct {
	ETag   string
	Digest [sha256.Size]byte
	ID     string
	// Set before replacing a clean native credential, whose native update is DELETE then POST.
	ReplacingETag string `json:",omitempty"`
}

type ConsoleSync struct {
	client   ConsoleClient
	source   BlobStore
	box      harness.Sandbox
	marker   string
	base     consoleBaseline
	redactor *diagnostic.Redactor
	nextRead time.Time
}

func NewOpenCode(ctx context.Context, box harness.Sandbox, stateDir string, driver ConsoleClient, source session.Credentials, redactor *diagnostic.Redactor) (Sync, error) {
	if source.Store == nil {
		return nil, errors.New("credential store is required")
	}
	store, err := OpenStore(ctx, *source.Store, source.Key)
	if err != nil {
		return nil, err
	}
	return &ConsoleSync{client: driver, source: store, box: box, marker: path.Join(stateDir, "orpheus-credential-version.json"), redactor: redactor}, nil
}

func (c *ConsoleSync) describe(raw []byte) (consoleBaseline, error) {
	entry, err := ParseConsole(raw)
	if err != nil {
		return consoleBaseline{}, err
	}
	c.redactor.Add(entry.Value.Access, entry.Value.Refresh)
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return consoleBaseline{}, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return consoleBaseline{}, err
	}
	return consoleBaseline{ID: entry.ID, Digest: sha256.Sum256(canonical)}, nil
}

func (c *ConsoleSync) remember(ctx context.Context, version consoleBaseline) error {
	if version.ETag == "" {
		return errors.New("credential object ETag is unavailable")
	}
	raw, err := json.Marshal(version)
	if err != nil {
		return err
	}
	if err = c.box.Write(ctx, c.marker+".tmp", raw); err != nil {
		return err
	}
	if _, err = c.box.Run(ctx, "mv -- "+harness.Quote(c.marker+".tmp")+" "+harness.Quote(c.marker)); err != nil {
		return err
	}
	c.base = version
	c.nextRead = time.Now().Add(5 * time.Second)
	return nil
}

func (c *ConsoleSync) readBaseline(ctx context.Context) (consoleBaseline, error) {
	var base consoleBaseline
	r, err := c.box.Read(ctx, c.marker)
	if err != nil {
		return base, err
	}
	defer func() { _ = r.Close() }()
	err = json.NewDecoder(io.LimitReader(r, 4096)).Decode(&base)
	if err == nil && (base.ID == "" || base.ETag == "" || base.Digest == [sha256.Size]byte{}) {
		return base, errors.New("invalid credential version marker")
	}
	return base, err
}

// Seed is called after the transport is ready. On a restored database the
// version marker distinguishes unflushed native rotation from an operator edit.
func (c *ConsoleSync) Seed(ctx context.Context) error {
	blob, err := c.source.Get(ctx)
	if err != nil {
		return err
	}
	remote, err := c.describe(blob.Raw)
	if err != nil {
		return err
	}
	remote.ETag = blob.ETag
	if remote.ETag == "" {
		return errors.New("credential object ETag is unavailable")
	}
	base, err := c.readBaseline(ctx)
	fresh := errors.Is(err, fs.ErrNotExist)
	if err != nil && !fresh {
		return err
	}
	if fresh {
		// A crash between the import and marker write can leave the row
		// present. ImportCredential requires matching data for that ID.
		if err = c.client.ImportCredential(ctx, blob.Raw, ""); err != nil {
			return err
		}
		return c.remember(ctx, remote)
	}
	native, err := c.client.ExportCredential(ctx)
	if err != nil {
		return err
	}
	if len(native) == 0 && ((base.ID == remote.ID && base.ETag == remote.ETag) || base.ReplacingETag == remote.ETag) {
		if err = c.client.ImportCredential(ctx, blob.Raw, ""); err != nil {
			return err
		}
		return c.remember(ctx, remote)
	}
	local, err := c.describe(native)
	if err != nil {
		return err
	}
	if local.Digest == remote.Digest {
		return c.remember(ctx, remote)
	}
	if base.ID != local.ID {
		return errors.New("sandbox credential identity changed")
	}
	if local.Digest == base.Digest {
		base.ReplacingETag = remote.ETag
		if err = c.remember(ctx, base); err != nil {
			return err
		}
		if err = c.client.ImportCredential(ctx, blob.Raw, base.ID); err != nil {
			return err
		}
		return c.remember(ctx, remote)
	}
	if base.ETag != remote.ETag || base.ID != remote.ID {
		return errors.New("concurrent account credential update")
	}
	return c.upload(ctx, native, local, base.ETag)
}

func (c *ConsoleSync) upload(ctx context.Context, raw []byte, version consoleBaseline, previous string) error {
	etag, err := c.source.Put(ctx, raw, previous)
	if err != nil {
		// A lost PUT acknowledgement or marker write leaves the old ETag in
		// the sandbox. Accept the remote version only if its contents match.
		blob, readErr := c.source.Get(ctx)
		if readErr != nil {
			return err
		}
		saved, parseErr := c.describe(blob.Raw)
		if parseErr != nil || saved.Digest != version.Digest {
			return err
		}
		etag = blob.ETag
	}
	version.ETag = etag
	return c.remember(ctx, version)
}

func (c *ConsoleSync) sync(ctx context.Context) error {
	raw, err := c.client.ExportCredential(ctx)
	if err != nil {
		return err
	}
	local, err := c.describe(raw)
	if err != nil {
		return err
	}
	if local.ID != c.base.ID {
		return errors.New("sandbox credential identity changed")
	}
	if local.Digest == c.base.Digest {
		return nil
	}
	return c.upload(ctx, raw, local, c.base.ETag)
}

func (c *ConsoleSync) Sync(ctx context.Context, force bool) {
	if !force && time.Now().Before(c.nextRead) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c.nextRead = time.Now().Add(5 * time.Second)
	if err := c.sync(ctx); err != nil {
		slog.WarnContext(ctx, "OpenCode credential synchronization failed")
	}
}
func (c *ConsoleSync) Watch(context.Context) error { return nil }
func (c *ConsoleSync) Close() error                { return nil }

func (c *ConsoleSync) Flush(ctx context.Context) error {
	if c.base.ID == "" {
		base, err := c.readBaseline(ctx)
		if err != nil {
			return err
		}
		c.base = base
	}
	return c.sync(ctx)
}
