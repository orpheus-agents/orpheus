package httpserver

import (
	"context"

	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/templates"
)

func (s *Server) GetTemplates(ctx context.Context, r api.GetTemplatesRequestObject) (api.GetTemplatesResponseObject, error) {
	out, err := s.Store.Templates(ctx, templates.Filter{Active: r.Params.Active, Name: r.Params.Name, NameContains: r.Params.NameContains}, value(r.Params.Limit, 50), value(r.Params.Cursor, ""))
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetTemplates200JSONResponse](out)
}

func (s *Server) GetTemplate(ctx context.Context, r api.GetTemplateRequestObject) (api.GetTemplateResponseObject, error) {
	out, err := s.Store.Template(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetTemplate200JSONResponse](out)
}

func (s *Server) CreateTemplate(ctx context.Context, r api.CreateTemplateRequestObject) (api.CreateTemplateResponseObject, error) {
	if r.Body == nil {
		return nil, templates.Invalid("body", "template_name", "required", "A template name is required.")
	}
	in, err := templates.Normalize(templates.Input{TemplateName: r.Body.TemplateName, Tag: r.Body.Tag, Description: r.Body.Description, Active: value(r.Body.Active, true)})
	if err != nil {
		return nil, err
	}
	providerID, err := s.templates.Verify(ctx, in)
	if err != nil {
		return nil, err
	}
	out, err := s.Store.CreateTemplate(ctx, in, providerID)
	if err != nil {
		return nil, err
	}
	if audit, ok := ctx.Value(templateAuditKey{}).(*templateAudit); ok {
		audit.id = out.ID.String()
	}
	return mapResponse[api.CreateTemplate201JSONResponse](out)
}

func (s *Server) UpdateTemplate(ctx context.Context, r api.UpdateTemplateRequestObject) (api.UpdateTemplateResponseObject, error) {
	if r.Body == nil {
		return nil, templates.Invalid("body", "", "required", "Provide active or description.")
	}
	patch, err := templates.ParsePatch(*r.Body)
	if err != nil {
		return nil, err
	}
	out, err := s.Store.UpdateTemplate(ctx, r.ID, patch)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.UpdateTemplate200JSONResponse](out)
}

func (s *Server) DeleteTemplate(ctx context.Context, r api.DeleteTemplateRequestObject) (api.DeleteTemplateResponseObject, error) {
	if err := s.Store.DeleteTemplate(ctx, r.ID); err != nil {
		return nil, err
	}
	return api.DeleteTemplate204Response{}, nil
}
