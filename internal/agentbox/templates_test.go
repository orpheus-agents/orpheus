package agentbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/templates"
)

func verifier(t *testing.T, handler http.HandlerFunc) *TemplateVerifier {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("AGENTBOX_API_URL", server.URL)
	t.Setenv("AGENTBOX_API_KEY", "private-provider-key")
	v, err := NewTemplateVerifier()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyTemplate(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  *string
		tags string
		want int
	}{
		{"SHORT", nil, `[{"tag":"default","buildID":"anything"}]`, 0},
		{"TEAM/NAME", new("V1"), `[{"tag":"v1"}]`, 0},
		{"name", nil, `[{"tag":"v1"}]`, 422},
		{"name", new("missing"), `[]`, 422},
		{"name", nil, `null`, 503},
		{"name", nil, `[{"buildID":"only"}]`, 503},
		{"name", nil, `{}`, 503},
	} {
		t.Run(tc.name+tc.tags, func(t *testing.T) {
			calls := 0
			v := verifier(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.Header.Get("X-API-KEY") != "private-provider-key" {
					t.Error(r.Method, r.Header)
				}
				switch calls {
				case 1:
					want := "/templates/aliases/" + strings.ReplaceAll(strings.ToLower(tc.name), "/", "%2F")
					if r.URL.EscapedPath() != want {
						t.Error(r.URL.EscapedPath(), want)
					}
					_, _ = fmt.Fprint(w, `{"templateID":"provider-id","public":true}`)
				case 2:
					if r.URL.Path != "/templates/provider-id/tags" {
						t.Error(r.URL.Path)
					}
					_, _ = fmt.Fprint(w, tc.tags)
				default:
					t.Error("unexpected provider call")
				}
			})
			id, err := v.Verify(t.Context(), templates.Input{TemplateName: tc.name, Tag: tc.tag})
			if tc.want == 0 {
				if err != nil || id != "provider-id" {
					t.Fatal(id, err)
				}
			} else {
				failure, ok := errors.AsType[*session.APIError](err)
				if !ok || failure.Status != tc.want {
					t.Fatal(err)
				}
			}
			if calls != 2 {
				t.Fatal(calls)
			}
		})
	}
}

func TestVerifyProviderFailures(t *testing.T) {
	for _, atTags := range []bool{false, true} {
		for _, status := range []int{400, 401, 403, 404, 429, 500, 502, 302} {
			t.Run(fmt.Sprintf("tags=%v/status=%d", atTags, status), func(t *testing.T) {
				v := verifier(t, func(w http.ResponseWriter, r *http.Request) {
					if atTags && strings.Contains(r.URL.Path, "/aliases/") {
						_, _ = fmt.Fprint(w, `{"templateID":"id"}`)
						return
					}
					w.Header().Set("Location", "/secret")
					w.WriteHeader(status)
					_, _ = fmt.Fprint(w, "private-provider-key SECRET_RESPONSE")
				})
				_, err := v.Verify(t.Context(), templates.Input{TemplateName: "ok"})
				failure, ok := errors.AsType[*session.APIError](err)
				want := 503
				if status == 400 || status == 403 || status == 404 {
					want = 422
				}
				if !ok || failure.Status != want || strings.Contains(fmt.Sprint(err), "SECRET") || strings.Contains(fmt.Sprint(err), "private-provider-key") {
					t.Fatal(err)
				}
			})
		}
	}
	for _, body := range []string{`{}`, `null`, `{"templateID":4}`, `{`, strings.Repeat("x", 1024*1024+1)} {
		v := verifier(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) })
		if _, err := v.Verify(t.Context(), templates.Input{TemplateName: "ok"}); err == nil {
			t.Fatal("malformed response accepted")
		}
	}
}

func TestVerificationDoesNotStartForInvalidTagOrMissingKey(t *testing.T) {
	v := verifier(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected HTTP call") })
	for _, in := range []templates.Input{{TemplateName: "ok", Tag: new(uuid.NewString())}, {TemplateName: "bad/name/tag"}} {
		if _, err := v.Verify(t.Context(), in); err == nil {
			t.Fatal(in)
		}
	}
	v.config.key = ""
	if _, err := v.Verify(t.Context(), templates.Input{TemplateName: "ok"}); err == nil {
		t.Fatal("missing key accepted")
	}
}

func TestVerificationTimeout(t *testing.T) {
	v := verifier(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	v.client.Timeout = 10 * time.Millisecond
	_, err := v.Verify(t.Context(), templates.Input{TemplateName: "ok"})
	failure, ok := errors.AsType[*session.APIError](err)
	if !ok || failure.Status != 503 {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := v.Verify(ctx, templates.Input{TemplateName: "ok"}); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestProviderConfiguration(t *testing.T) {
	for _, tc := range []struct{ domain, debug, explicit, want string }{
		{"", "", "", "https://api.agentbox-runtime.ru"},
		{"custom.test", "false", "", "https://api.custom.test"},
		{"custom.test", "true", "", "http://localhost:3000"},
		{"custom.test", "true", "https://example.test/api/", "https://example.test/api"},
	} {
		t.Setenv("AGENTBOX_DOMAIN", tc.domain)
		t.Setenv("AGENTBOX_DEBUG", tc.debug)
		t.Setenv("AGENTBOX_API_URL", tc.explicit)
		cfg, err := loadProviderConfig()
		if err != nil || cfg.apiURL != tc.want {
			t.Fatal(cfg.apiURL, err)
		}
	}
	t.Setenv("AGENTBOX_DEBUG", "typo")
	if _, err := loadProviderConfig(); err == nil {
		t.Fatal("invalid debug accepted")
	}
	t.Setenv("AGENTBOX_DEBUG", "false")
	for _, url := range []string{"relative", "ftp://example.test", "https://secret@example.test", "https://example.test?q=1"} {
		t.Setenv("AGENTBOX_API_URL", url)
		if _, err := loadProviderConfig(); err == nil {
			t.Fatal(url)
		}
	}
}
