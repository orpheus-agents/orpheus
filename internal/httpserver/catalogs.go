package httpserver

import (
	"context"
	"maps"
	"slices"

	"github.com/orpheus-agents/orpheus/internal/api"
)

// Map only public fields; authentication and credential sources never enter DTOs.
func (s *Server) GetProfiles(_ context.Context, _ api.GetProfilesRequestObject) (api.GetProfilesResponseObject, error) {
	items := make([]api.Profile, 0, len(s.Store.Profiles.Profiles))
	for _, name := range slices.Sorted(maps.Keys(s.Store.Profiles.Profiles)) {
		profile := s.Store.Profiles.Profiles[name]
		options := api.CodexProfile{}
		if profile.Codex.Effort != "" {
			options.Effort = new(api.CodexProfileEffort(profile.Codex.Effort))
		}
		if profile.Codex.Summary != "" {
			options.Summary = new(api.CodexProfileSummary(profile.Codex.Summary))
		}
		if profile.Codex.Personality != "" {
			options.Personality = new(api.CodexProfilePersonality(profile.Codex.Personality))
		}
		if profile.Codex.ServiceTier != "" {
			options.ServiceTier = new(profile.Codex.ServiceTier)
		}
		items = append(items, api.Profile{
			Name: name, Description: profile.Description, Harness: api.ProfileHarness(profile.Harness),
			Model: profile.Model, Codex: options, Instructions: profile.Instructions,
		})
	}
	return api.GetProfiles200JSONResponse{Items: items}, nil
}

func (s *Server) GetTemplates(_ context.Context, _ api.GetTemplatesRequestObject) (api.GetTemplatesResponseObject, error) {
	items := make([]api.Template, 0, len(s.Store.Profiles.Templates))
	for _, name := range slices.Sorted(maps.Keys(s.Store.Profiles.Templates)) {
		items = append(items, api.Template{Name: name, Description: s.Store.Profiles.Templates[name].Description})
	}
	return api.GetTemplates200JSONResponse{Items: items}, nil
}
