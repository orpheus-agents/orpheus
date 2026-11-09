package httpserver

import (
	"context"

	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func (s *Server) ConfirmServiceAccess(ctx context.Context, r api.ConfirmServiceAccessRequestObject) (api.ConfirmServiceAccessResponseObject, error) {
	if r.Body == nil {
		return nil, session.Problem(422, "validation_error", "Request body is required.")
	}
	if err := s.Store.ConfirmInvocation(ctx, r.Body.InvocationID); err != nil {
		return nil, err
	}
	return api.ConfirmServiceAccess200JSONResponse{InvocationID: r.Body.InvocationID}, nil
}
