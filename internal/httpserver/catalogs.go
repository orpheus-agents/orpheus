package httpserver

import (
	"context"
	"maps"
	"slices"

	"github.com/orpheus-agents/orpheus/internal/api"
)

func (s *Server) GetServices(_ context.Context, _ api.GetServicesRequestObject) (api.GetServicesResponseObject, error) {
	items := make([]api.Service, 0, len(s.Store.Deployment.Services))
	for _, code := range slices.Sorted(maps.Keys(s.Store.Deployment.Services)) {
		service := s.Store.Deployment.Services[code]
		items = append(items, api.Service{Code: code, Name: service.Name, Description: service.Description, EnvNames: service.EnvNames()})
	}
	return api.GetServices200JSONResponse{Items: items}, nil
}
