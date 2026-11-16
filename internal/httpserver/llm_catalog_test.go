//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/client"
	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestLLMCatalogHTTP(t *testing.T) {
	server, _ := testServer(t)
	raw := externalRequest(t, server, "POST", "/api/v1/llm-accounts", `{"name":"First","harness":"codex","auth_mode":"api_key","api_key":"private-example-key"}`, "", 201)
	account := decodeHTTP[llm.AccountDetails](t, raw)
	if strings.Contains(string(raw), "private-example-key") || account.APIKeyMask == nil {
		t.Fatal("invalid secret response")
	}
	path := "/api/v1/llm-accounts/" + account.ID.String()
	externalRequest(t, server, "GET", path, "", "", 200)
	listed := externalRequest(t, server, "GET", "/api/v1/llm-accounts?name=First", "", "", 200)
	if strings.Contains(string(listed), "api_key_mask") {
		t.Fatal("mask leaked to list")
	}
	for _, body := range []string{`{}`, `{"active":null}`, `{"auth_mode":"account"}`, `{"harness":"codex"}`, `{"credential_store":"s"}`, `{"api_key":"other"}`} {
		externalRequest(t, server, "PATCH", path, body, "", 422)
	}
	externalRequest(t, server, "PATCH", path, `{"name":"Renamed","active":false}`, "", 200)
	externalRequest(t, server, "PUT", path+"/api-key", `{"api_key":"replacement-key"}`, "", 204)
	externalRequest(t, server, "PUT", path+"/api-key", `{"api_key":" "}`, "", 422)
	externalRequest(t, server, "PUT", path+"/api-key", `{"api_key":"a","api_key":"b"}`, "", 400)
	profileRaw := externalRequest(t, server, "POST", "/api/v1/profiles", fmt.Sprintf(`{"name":"custom","account_id":%q,"model":"model"}`, account.ID), "", 201)
	profile := decodeHTTP[llm.Profile](t, profileRaw)
	if profile.Available || !profile.Active || profile.AccountActive {
		t.Fatal(profile)
	}
	pp := "/api/v1/profiles/" + profile.ID.String()
	for _, body := range []string{`{}`, `{"name":"other"}`, `{"model":""}`, `{"model":null}`, `{"instructions":null}`, `{"codex":{"effort":"invalid"}}`} {
		externalRequest(t, server, "PATCH", pp, body, "", 422)
	}
	externalRequest(t, server, "PATCH", pp, `{"codex":{"effort":"high"},"instructions":"test"}`, "", 200)
	externalRequest(t, server, "PATCH", pp, `{"codex":{"effort":null}}`, "", 200)
	externalRequest(t, server, "DELETE", path, "", "", 409)
	externalRequest(t, server, "DELETE", pp, "", "", 204)
	externalRequest(t, server, "DELETE", path, "", "", 204)
	externalRequest(t, server, "GET", path, "", "", 404)
	for _, base := range []string{"/api/v1/profiles", "/api/v1/llm-accounts"} {
		for _, query := range []string{"limit=201", "limit=0", "name=%20", "name=x&name_contains=x", "cursor=bad", "active=bad"} {
			externalRequest(t, server, "GET", base+"?"+query, "", "", 422)
		}
	}
	for _, query := range []string{"account_ids=bad", "account_ids=", "account_ids=" + strings.Repeat(uuid.NewString()+",", 200) + uuid.NewString()} {
		externalRequest(t, server, "GET", "/api/v1/accounts/limits?"+query, "", "", 422)
	}
}
func TestLLMAuthFileConditionalWrites(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("TEST_S3_ENDPOINT required")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "orpheus-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "orpheus-test-password")
	t.Setenv("AWS_SESSION_TOKEN", "")
	remote := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("orpheus-test", "orpheus-test-password", "")}, func(o *s3.Options) { o.BaseEndpoint = &endpoint; o.UsePathStyle = true })
	bucket := "orpheus-" + uuid.NewString()
	if _, err := remote.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatal(err)
	}
	key := ""
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if key != "" {
			_, _ = remote.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key})
		}
		_, _ = remote.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket})
	})
	server, storage := testServer(t)
	storage.Deployment.CredentialStores = map[string]session.CredentialStore{"s": {Type: "s3", Region: "us-east-1", Bucket: bucket, EndpointURL: &endpoint}}
	externalRequest(t, server, "GET", "/api/v1/credential-stores", "", "", 200)
	c, err := client.NewClientWithResponses(server.URL, client.WithHTTPClient(server.Client()), client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer key")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	auth := `{"tokens":{"access_token":"access","refresh_token":"refresh","id_token":"id"},"future":{"unknown":123}}`
	created, err := c.CreateLlmAccountWithResponse(t.Context(), json.RawMessage(`{"name":"File","harness":"codex","auth_mode":"account","credential_store":"s","auth_file":`+auth+`}`))
	if err != nil || created.JSON201 == nil {
		t.Fatal(created, err)
	}
	id := created.JSON201.ID
	key = "llm-accounts/" + id.String() + "/auth.json"
	got, err := c.GetAuthFileWithResponse(t.Context(), id)
	if err != nil || got.StatusCode() != 200 || got.Headers200 == nil || got.Headers200.ETag == "" || got.Headers200.CacheControl != "no-store" {
		t.Fatal(got, err)
	}
	assertCatalogJSON(t, got.Body, auth)
	etag := got.Headers200.ETag
	next := strings.Replace(auth, `"access"`, `"rotated"`, 1)
	saved, err := c.ReplaceAuthFileWithResponse(t.Context(), id, &client.ReplaceAuthFileParams{IfMatch: etag}, json.RawMessage(next))
	if err != nil || saved.StatusCode() != 204 {
		t.Fatal(saved, err)
	}
	stale, err := c.ReplaceAuthFileWithResponse(t.Context(), id, &client.ReplaceAuthFileParams{IfMatch: etag}, json.RawMessage(auth))
	if err != nil || stale.StatusCode() != 409 {
		t.Fatal(stale, err)
	}
	got, err = c.GetAuthFileWithResponse(t.Context(), id)
	if err != nil || got.StatusCode() != 200 {
		t.Fatal(got, err)
	}
	assertCatalogJSON(t, got.Body, next)
	filePath := "/api/v1/llm-accounts/" + id.String() + "/auth-file"
	externalRequest(t, server, "PUT", filePath, auth, "", 422) // Missing precondition.
	force, err := c.ReplaceAuthFileWithResponse(t.Context(), id, &client.ReplaceAuthFileParams{IfMatch: "*"}, json.RawMessage(auth))
	if err != nil || force.StatusCode() != 422 {
		t.Fatal(force, err)
	}
	filtered, err := c.GetAccountLimitsWithResponse(t.Context(), &client.GetAccountLimitsParams{AccountIds: &[]uuid.UUID{id}})
	if err != nil || filtered.JSON200 == nil || len(filtered.JSON200.Items) != 1 {
		t.Fatal(filtered, err)
	}
	filtered, err = c.GetAccountLimitsWithResponse(t.Context(), &client.GetAccountLimitsParams{AccountIds: &[]uuid.UUID{uuid.New()}})
	if err != nil || filtered.JSON200 == nil || len(filtered.JSON200.Items) != 0 {
		t.Fatal(filtered, err)
	}
	if err := storage.DeleteAccount(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: &bucket, Key: &key}); err != nil {
		t.Fatal("deletion removed S3 auth", err)
	}
}
func TestLLMBrowserMutationOrigins(t *testing.T) {
	_, storage := testServer(t)
	storage.Settings.BrowserAuth.Mode = "anonymous"
	storage.Settings.BrowserAuth.PublicURL = "http://orpheus.test"
	handler, err := Handler(storage, t.Context(), NewHTTPMetrics())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/llm-accounts", "/api/v1/profiles"} {
		for _, origin := range []string{"", "http://evil.test", "http://orpheus.test"} {
			req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", origin)
			req.Header.Set("X-Orpheus-CSRF", "1")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			want := 403
			if origin == "http://orpheus.test" {
				want = 422
			}
			if w.Code != want {
				t.Fatal(path, origin, w.Code, w.Body.String())
			}
		}
	}
}
