package httpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/browserauth"
	"github.com/orpheus-agents/orpheus/internal/session"
)

type browserRequestKey struct{}
type browserIdentityKey struct{}

func browserReadable(operation string) bool {
	switch operation {
	case "GetAnalyticsOverview", "ListSessions", "GetSession", "ListAllRuns", "ListRuns", "GetRun", "GetHistory", "GetEvents", "StreamEvents":
		return true
	default:
		return false
	}
}
func authorize(r *http.Request, auth *browserauth.Service, keys []string, operation string) (*browserauth.Identity, error) {
	unauthorized := session.Problem(401, "unauthorized", "A valid credential is required.")
	if _, present := r.Header["Authorization"]; present {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		authorized := 0
		if ok && strings.EqualFold(scheme, "Bearer") && token != "" && len(r.Header.Values("Authorization")) == 1 {
			for _, key := range keys {
				authorized |= subtle.ConstantTimeCompare([]byte(token), []byte(key))
			}
		}
		if authorized == 0 {
			return nil, unauthorized
		}
		return nil, nil
	}
	if auth.Mode() == "anonymous" && browserReadable(operation) {
		return nil, nil
	}
	if auth.Mode() != "saml" {
		return nil, unauthorized
	}
	identity, err := auth.Authenticate(r)
	if errors.Is(err, browserauth.ErrNoSession) {
		return nil, unauthorized
	}
	if err != nil {
		return nil, err
	}
	if !browserReadable(operation) {
		return nil, session.Problem(403, "read_only_access", "Browser sessions permit reading only.")
	}
	return identity, nil
}

// Only auth visitors need the raw HTTP request for cookies and SAML forms.
func browserRequestMiddleware(next api.StrictHandlerFunc, operation string) api.StrictHandlerFunc {
	switch operation {
	case "AuthSession", "BrowserLogin", "BrowserCallback", "BrowserLogout", "SamlMetadata":
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			return next(context.WithValue(ctx, browserRequestKey{}, r), w, r, request)
		}
	default:
		return next
	}
}

// The custom response visitor preserves Set-Cookie, redirects and the SAML form
// protocol while all routes and response types remain generated from OpenAPI.
type browserResponse struct {
	serve func(http.ResponseWriter) error
}

func (b browserResponse) VisitAuthSessionResponse(w http.ResponseWriter) error     { return b.serve(w) }
func (b browserResponse) VisitBrowserLoginResponse(w http.ResponseWriter) error    { return b.serve(w) }
func (b browserResponse) VisitBrowserCallbackResponse(w http.ResponseWriter) error { return b.serve(w) }
func (b browserResponse) VisitBrowserLogoutResponse(w http.ResponseWriter) error   { return b.serve(w) }
func (b browserResponse) VisitSamlMetadataResponse(w http.ResponseWriter) error    { return b.serve(w) }
func (s *Server) browserResponse(ctx context.Context, handle func(http.ResponseWriter, *http.Request) error) browserResponse {
	return browserResponse{serve: func(w http.ResponseWriter) error {
		r := ctx.Value(browserRequestKey{}).(*http.Request)
		// Keep the raw request/body, with the current handler context.
		return handle(w, r.WithContext(ctx))
	}}
}
func (s *Server) AuthSession(ctx context.Context, _ api.AuthSessionRequestObject) (api.AuthSessionResponseObject, error) {
	return s.browserResponse(ctx, func(w http.ResponseWriter, r *http.Request) error {
		state, err := s.auth.State(w, r)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(state)
	}), nil
}
func (s *Server) BrowserLogin(ctx context.Context, _ api.BrowserLoginRequestObject) (api.BrowserLoginResponseObject, error) {
	return s.browserResponse(ctx, s.auth.Login), nil
}
func (s *Server) BrowserCallback(ctx context.Context, _ api.BrowserCallbackRequestObject) (api.BrowserCallbackResponseObject, error) {
	return s.browserResponse(ctx, s.auth.Callback), nil
}
func (s *Server) BrowserLogout(ctx context.Context, _ api.BrowserLogoutRequestObject) (api.BrowserLogoutResponseObject, error) {
	return s.browserResponse(ctx, s.auth.Logout), nil
}
func (s *Server) SamlMetadata(ctx context.Context, _ api.SamlMetadataRequestObject) (api.SamlMetadataResponseObject, error) {
	return s.browserResponse(ctx, s.auth.Metadata), nil
}

func watchBrowserSession(parent context.Context, auth *browserauth.Service, identity *browserauth.Identity, interval time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(parent, identity.ExpiresAt)
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			// Leave room for a bounded DB check within the 30-second revocation limit.
			case <-time.After(interval):
				probe, stop := context.WithTimeout(ctx, 5*time.Second)
				_, err := auth.Identity(probe, identity.TokenHash)
				stop()
				if err != nil {
					return
				}
			}
		}
	}()
	return ctx, cancel
}
