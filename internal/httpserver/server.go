// Package httpserver implements the generated HTTP contract.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/skillum-ai/orpheus/internal/api"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
)

type Server struct {
	Store   *store.Store
	streams context.Context
}

var _ api.StrictServerInterface = (*Server)(nil)

func value[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

// mapResponse keeps the domain independent of generated transport DTOs. Both
// representations are checked against the API schema in integration tests.
func mapResponse[T any](source any) (T, error) {
	var target T
	b, err := json.Marshal(source)
	if err != nil {
		return target, err
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	err = decoder.Decode(&target)
	return target, err
}
func key(p *string) (uuid.UUID, error) {
	id, err := uuid.Parse(value(p, ""))
	if err != nil {
		problem := session.Problem(422, "idempotency_key_required", "Idempotency-Key must be a UUID.")
		code := "invalid_value"
		if p == nil || *p == "" {
			code = "required"
		}
		problem.Problem.Details = []session.Detail{{Path: []any{"header", "Idempotency-Key"}, Code: code}}
		return uuid.Nil, problem
	}
	return id, nil
}
func location(a session.Acceptance) string {
	return fmt.Sprintf("/api/v1/sessions/%s/runs/%s", a.SessionID, a.RunID)
}
func (s *Server) CreateSession(ctx context.Context, r api.CreateSessionRequestObject) (api.CreateSessionResponseObject, error) {
	id, err := key(r.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, session.Problem(422, "validation_error", "Request body is required.")
	}
	in := session.CreateSession{Configuration: session.ConfigurationInput{Limits: session.Limits{RunTimeoutSeconds: 3600}}}
	b, err := json.Marshal(r.Body)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	a, err := s.Store.Accept(ctx, store.Admission{Create: &in, Key: id})
	if err != nil {
		return nil, err
	}
	body, err := mapResponse[api.Accepted](a)
	if err != nil {
		return nil, err
	}
	return api.CreateSession202JSONResponse{Body: body, Headers: api.CreateSession202ResponseHeaders{Location: location(a)}}, nil
}
func (s *Server) CreateRun(ctx context.Context, r api.CreateRunRequestObject) (api.CreateRunResponseObject, error) {
	id, err := key(r.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, session.Problem(422, "validation_error", "Request body is required.")
	}
	a, err := s.Store.Accept(ctx, store.Admission{SessionID: r.Sid, Text: r.Body.Message.Text, MessageExternalKey: r.Body.Message.ExternalKey, InputFingerprint: r.Body.InputFingerprint, Env: value(r.Body.Env, nil), EnvFrom: value(r.Body.EnvFrom, nil), Key: id})
	if err != nil {
		return nil, err
	}
	body, err := mapResponse[api.Accepted](a)
	if err != nil {
		return nil, err
	}
	return api.CreateRun202JSONResponse{Body: body, Headers: api.CreateRun202ResponseHeaders{Location: location(a)}}, nil
}
func (s *Server) SendMessage(ctx context.Context, r api.SendMessageRequestObject) (api.SendMessageResponseObject, error) {
	id, err := key(r.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, session.Problem(422, "validation_error", "Request body is required.")
	}
	a, err := s.Store.Accept(ctx, store.Admission{SessionID: r.Sid, RunID: r.Rid, Text: r.Body.Message.Text, MessageExternalKey: r.Body.Message.ExternalKey, Key: id})
	if err != nil {
		return nil, err
	}
	body, err := mapResponse[api.Accepted](a)
	if err != nil {
		return nil, err
	}
	return api.SendMessage202JSONResponse{Body: body, Headers: api.SendMessage202ResponseHeaders{Location: location(a)}}, nil
}
func (s *Server) CancelRun(ctx context.Context, r api.CancelRunRequestObject) (api.CancelRunResponseObject, error) {
	out, err := s.Store.Cancel(ctx, r.Sid, r.Rid)
	if err != nil {
		return nil, err
	}
	if out.Status.Terminal() {
		return mapResponse[api.CancelRun200JSONResponse](out)
	}
	return mapResponse[api.CancelRun202JSONResponse](out)
}
func (s *Server) GetSession(ctx context.Context, r api.GetSessionRequestObject) (api.GetSessionResponseObject, error) {
	v, err := s.Store.Session(ctx, r.Sid)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetSession200JSONResponse](v)
}
func (s *Server) GetRun(ctx context.Context, r api.GetRunRequestObject) (api.GetRunResponseObject, error) {
	v, err := s.Store.Run(ctx, r.Sid, r.Rid)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetRun200JSONResponse](v)
}
func (s *Server) ListSessions(ctx context.Context, r api.ListSessionsRequestObject) (api.ListSessionsResponseObject, error) {
	v, err := s.Store.ListSessions(ctx, value(r.Params.Limit, 50), value(r.Params.Cursor, ""), store.ListFilter{Namespace: r.Params.Namespace, ExternalKey: r.Params.ExternalKey, Status: stringPointer(r.Params.Status), Order: string(value(r.Params.Order, "asc"))})
	if err != nil {
		return nil, err
	}
	return mapResponse[api.ListSessions200JSONResponse](v)
}
func (s *Server) ListRuns(ctx context.Context, r api.ListRunsRequestObject) (api.ListRunsResponseObject, error) {
	v, err := s.Store.ListRuns(ctx, r.Sid, value(r.Params.Limit, 50), value(r.Params.Cursor, ""), store.ListFilter{InputFingerprint: r.Params.InputFingerprint, Status: stringPointer(r.Params.Status), Order: string(value(r.Params.Order, "asc"))})
	if err != nil {
		return nil, err
	}
	return mapResponse[api.ListRuns200JSONResponse](v)
}
func (s *Server) GetEvents(ctx context.Context, r api.GetEventsRequestObject) (api.GetEventsResponseObject, error) {
	v, err := s.Store.Events(ctx, r.Sid, value(r.Params.After, "0"), value(r.Params.Limit, 50))
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetEvents200JSONResponse](v)
}
func (s *Server) GetHistory(ctx context.Context, r api.GetHistoryRequestObject) (api.GetHistoryResponseObject, error) {
	v, err := s.Store.History(ctx, r.Sid, r.Params.RunID, value(r.Params.Limit, 50), value(r.Params.Cursor, ""), r.Params.MessageExternalKey)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetHistory200JSONResponse](v)
}
func (s *Server) Health(context.Context, api.HealthRequestObject) (api.HealthResponseObject, error) {
	return api.Health200JSONResponse{Status: "ok"}, nil
}
func (s *Server) Ready(ctx context.Context, _ api.ReadyRequestObject) (api.ReadyResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Store.Settings.ReadinessTimeout)
	defer cancel()
	if err := s.Store.Pool.Ping(ctx); err != nil {
		return api.Ready503JSONResponse{Status: api.UnavailableResponseStatusUnavailable}, nil
	}
	return api.Ready200JSONResponse{Status: "ok"}, nil
}
func (s *Server) StreamEvents(ctx context.Context, r api.StreamEventsRequestObject) (api.StreamEventsResponseObject, error) {
	after := value(r.Params.LastEventID, value(r.Params.After, "0"))
	if _, err := s.Store.Events(ctx, r.Sid, after, 1); err != nil {
		return nil, err
	}
	return &eventStream{ctx: ctx, streams: s.streams, store: s.Store, id: r.Sid, after: after}, nil
}

// A custom visitor allows a deadline on each write without imposing a total
// lifetime limit on SSE. No database transaction spans a network write.
type eventStream struct {
	ctx     context.Context
	streams context.Context
	store   *store.Store
	id      uuid.UUID
	after   string
}

func (e *eventStream) VisitStreamEventsResponse(w http.ResponseWriter) error {
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	control := http.NewResponseController(w)
	shutdownDone := make(chan struct{})
	stop := context.AfterFunc(e.streams, func() {
		defer close(shutdownDone)
		cancel()
		// Wake a client-blocked write as well as an idle stream or database read.
		_ = control.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stop() {
			<-shutdownDone
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	for {
		page, err := e.store.Events(ctx, e.id, e.after, 100)
		if err != nil {
			return err
		}
		if err := control.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(page.Items) == 0 {
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return err
			}
		}
		for _, event := range page.Items {
			raw, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", event.ID, event.Type, raw); err != nil {
				return err
			}
		}
		if err := control.Flush(); err != nil {
			return err
		}
		e.after = page.NextCursor
		if !page.HasMore {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
}

func stringPointer[T ~string](p *T) *string {
	if p == nil {
		return nil
	}
	return new(string(*p))
}
func (s *Server) ListAllRuns(ctx context.Context, r api.ListAllRunsRequestObject) (api.ListAllRunsResponseObject, error) {
	v, err := s.Store.ListAllRuns(ctx, value(r.Params.Limit, 50), value(r.Params.Cursor, ""), store.ListFilter{Namespace: r.Params.Namespace, ExternalKey: r.Params.ExternalKey, InputFingerprint: r.Params.InputFingerprint, Status: stringPointer(r.Params.Status), Order: string(value(r.Params.Order, "asc"))})
	if err != nil {
		return nil, err
	}
	return mapResponse[api.ListAllRuns200JSONResponse](v)
}
