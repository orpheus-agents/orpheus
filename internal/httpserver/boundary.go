package httpserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

type responseWriter struct {
	http.ResponseWriter
	written bool
}

func (w *responseWriter) WriteHeader(code int) { w.written = true; w.ResponseWriter.WriteHeader(code) }
func (w *responseWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func writeError(w http.ResponseWriter, err error) {
	if tracked, ok := w.(*responseWriter); ok && tracked.written {
		return
	}
	problem, ok := errors.AsType[*session.APIError](err)
	if !ok {
		problem = session.Problem(503, "storage_unavailable", "Storage is temporarily unavailable.")
	}
	if problem.Problem.Details == nil {
		problem.Problem.Details = []session.Detail{}
	}
	w.Header().Set("Content-Type", "application/json")
	if problem.Status == 401 {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(problem.Status)
	_ = json.NewEncoder(w).Encode(struct {
		Error session.Error `json:"error"`
	}{problem.Problem})
}
func Handler(s *store.Store, streams context.Context) (http.Handler, error) {
	spec, err := api.GetSpec()
	if err != nil {
		return nil, err
	}
	spec.Servers = nil
	router, err := legacy.NewRouter(spec)
	if err != nil {
		return nil, err
	}
	validationError := func(w http.ResponseWriter, _ *http.Request, err error) {
		writeError(w, validationProblem(err))
	}
	strict := api.NewStrictHandlerWithOptions(&Server{Store: s, streams: streams}, nil, api.StrictHTTPServerOptions{RequestErrorHandlerFunc: validationError, ResponseErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) { writeError(w, err) }})
	generated := api.HandlerWithOptions(strict, api.StdHTTPServerOptions{ErrorHandlerFunc: validationError})
	return http.HandlerFunc(func(base http.ResponseWriter, r *http.Request) {
		w := &responseWriter{ResponseWriter: base}
		if r.URL.Path == "/openapi.json" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(spec)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1") {
			scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			authorized := 0
			if ok && strings.EqualFold(scheme, "Bearer") {
				for _, key := range s.Settings.PublicAPIKeys {
					authorized |= subtle.ConstantTimeCompare([]byte(token), []byte(key))
				}
			}
			if authorized == 0 {
				writeError(w, session.Problem(401, "unauthorized", "A valid Bearer key is required."))
				return
			}
			if r.Method == http.MethodPost {
				raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.Settings.MaxRequestBytes))
				if err != nil {
					if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
						writeError(w, session.Problem(413, "request_too_large", "Request body is too large."))
					} else {
						writeError(w, session.Problem(400, "invalid_json", "Invalid request body."))
					}
					return
				}
				if len(raw) > 0 {
					media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
					if err != nil || media != "application/json" {
						writeError(w, session.Problem(415, "unsupported_media_type", "Invalid request body."))
						return
					}
					if strings.HasSuffix(r.URL.Path, "/cancel") || validateJSON(raw) != nil {
						writeError(w, session.Problem(400, "invalid_json", "Invalid request body."))
						return
					}
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
			}
			route, params, err := router.FindRoute(r)
			if err == nil {
				query := r.URL.Query()
				for _, parameter := range route.Operation.Parameters {
					p := parameter.Value
					if p != nil && p.In == "query" && len(query[p.Name]) > 1 {
						problem := session.Problem(422, "validation_error", "Query parameters must occur once.")
						problem.Problem.Details = []session.Detail{{Path: []any{"query", p.Name}, Code: "invalid_value"}}
						writeError(w, problem)
						return
					}
				}
				input := &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route, Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, SkipSettingDefaults: true}}
				if err := openapi3filter.ValidateRequest(r.Context(), input); err != nil {
					validationError(w, r, err)
					return
				}
			}
		}
		generated.ServeHTTP(w, r)
	}), nil
}

func validationProblem(err error) *session.APIError {
	p := session.Problem(422, "validation_error", "Request validation failed.")
	detail := session.Detail{Path: []any{"body"}, Code: "invalid_value"}
	// Generated decoding can reject integers that schema validation accepted
	// after float64 rounding. Keep the field path without exposing its value.
	if decoded, ok := errors.AsType[*json.UnmarshalTypeError](err); ok && decoded.Field != "" {
		for segment := range strings.SplitSeq(decoded.Field, ".") {
			detail.Path = append(detail.Path, segment)
		}
	}
	if request, ok := errors.AsType[*openapi3filter.RequestError](err); ok && request.Parameter != nil {
		detail.Path = []any{request.Parameter.In, request.Parameter.Name}
		if errors.Is(err, openapi3filter.ErrInvalidRequired) {
			detail.Code = "required"
		}
	}
	if schema, ok := errors.AsType[*openapi3.SchemaError](err); ok {
		for _, segment := range schema.JSONPointer() {
			if index, err := strconv.Atoi(segment); err == nil {
				detail.Path = append(detail.Path, index)
			} else {
				detail.Path = append(detail.Path, segment)
			}
		}
		switch schema.SchemaField {
		case "type":
			detail.Code = "invalid_type"
		case "required":
			detail.Code = "required"
		case "additionalProperties":
			detail.Code = "unknown_field"
		}
	}
	p.Problem.Details = []session.Detail{detail}
	return p
}

// encoding/json replaces malformed UTF-8 and accepts duplicate object keys.
// Reject both before schema validation or generated decoding loses information.
func validateJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 256 {
			return errors.New("JSON nesting limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("duplicate JSON key")
				}
				keys[name] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}
