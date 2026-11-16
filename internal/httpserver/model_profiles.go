package httpserver

import (
	"context"

	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/llm"
)

func (s *Server) GetProfiles(ctx context.Context, r api.GetProfilesRequestObject) (api.GetProfilesResponseObject, error) {
	out, err := s.Store.ModelProfiles(ctx, llm.Filter{Active: r.Params.Active, Available: r.Params.Available, Name: r.Params.Name, NameContains: r.Params.NameContains}, value(r.Params.Limit, 50), value(r.Params.Cursor, ""))
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetProfiles200JSONResponse](out)
}
func (s *Server) GetProfile(ctx context.Context, r api.GetProfileRequestObject) (api.GetProfileResponseObject, error) {
	out, err := s.Store.Profile(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetProfile200JSONResponse](out)
}
func (s *Server) CreateProfile(ctx context.Context, r api.CreateProfileRequestObject) (api.CreateProfileResponseObject, error) {
	if r.Body == nil {
		return nil, llm.Invalid("")
	}
	in, err := mapResponse[llm.ProfileInput](r.Body)
	if err != nil {
		return nil, err
	}
	out, err := s.Store.CreateProfile(ctx, in)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.CreateProfile201JSONResponse](out)
}
func (s *Server) UpdateProfile(ctx context.Context, r api.UpdateProfileRequestObject) (api.UpdateProfileResponseObject, error) {
	if r.Body == nil {
		return nil, llm.Invalid("")
	}
	out, err := s.Store.UpdateProfile(ctx, r.ID, *r.Body)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.UpdateProfile200JSONResponse](out)
}
func (s *Server) DeleteProfile(ctx context.Context, r api.DeleteProfileRequestObject) (api.DeleteProfileResponseObject, error) {
	if err := s.Store.DeleteProfile(ctx, r.ID); err != nil {
		return nil, err
	}
	return api.DeleteProfile204Response{}, nil
}
