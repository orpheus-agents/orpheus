package httpserver

import (
	"context"
	"maps"
	"slices"

	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/llm"
)

func (s *Server) GetLlmAccounts(ctx context.Context, r api.GetLlmAccountsRequestObject) (api.GetLlmAccountsResponseObject, error) {
	out, err := s.Store.Accounts(ctx, llm.Filter{Active: r.Params.Active, Name: r.Params.Name, NameContains: r.Params.NameContains}, value(r.Params.Limit, 50), value(r.Params.Cursor, ""))
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetLlmAccounts200JSONResponse](out)
}
func (s *Server) GetLlmAccount(ctx context.Context, r api.GetLlmAccountRequestObject) (api.GetLlmAccountResponseObject, error) {
	out, err := s.Store.Account(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.GetLlmAccount200JSONResponse](out)
}
func (s *Server) CreateLlmAccount(ctx context.Context, r api.CreateLlmAccountRequestObject) (api.CreateLlmAccountResponseObject, error) {
	if r.Body == nil {
		return nil, llm.Invalid("")
	}
	var in llm.AccountInput
	if err := llm.Decode(*r.Body, &in); err != nil {
		return nil, err
	}
	out, err := s.Store.CreateAccount(ctx, in)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.CreateLlmAccount201JSONResponse](out)
}
func (s *Server) UpdateLlmAccount(ctx context.Context, r api.UpdateLlmAccountRequestObject) (api.UpdateLlmAccountResponseObject, error) {
	if r.Body == nil {
		return nil, llm.Invalid("")
	}
	out, err := s.Store.UpdateAccount(ctx, r.ID, *r.Body)
	if err != nil {
		return nil, err
	}
	return mapResponse[api.UpdateLlmAccount200JSONResponse](out)
}
func (s *Server) DeleteLlmAccount(ctx context.Context, r api.DeleteLlmAccountRequestObject) (api.DeleteLlmAccountResponseObject, error) {
	if err := s.Store.DeleteAccount(ctx, r.ID); err != nil {
		return nil, err
	}
	return api.DeleteLlmAccount204Response{}, nil
}
func (s *Server) ReplaceAPIKey(ctx context.Context, r api.ReplaceAPIKeyRequestObject) (api.ReplaceAPIKeyResponseObject, error) {
	if r.Body == nil {
		return nil, llm.Invalid("api_key")
	}
	if err := s.Store.ReplaceAccountKey(ctx, r.ID, value(r.Body.APIKey, "")); err != nil {
		return nil, err
	}
	return api.ReplaceAPIKey204Response{}, nil
}
func (s *Server) GetAuthFile(ctx context.Context, r api.GetAuthFileRequestObject) (api.GetAuthFileResponseObject, error) {
	file, err := s.Store.ReadAccountFile(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	return api.GetAuthFile200JSONResponse{Body: file.Raw, Headers: api.GetAuthFile200ResponseHeaders{ETag: file.ETag, CacheControl: "no-store"}}, nil
}
func (s *Server) ReplaceAuthFile(ctx context.Context, r api.ReplaceAuthFileRequestObject) (api.ReplaceAuthFileResponseObject, error) {
	if r.Body == nil {
		return nil, llm.Invalid("auth_file")
	}
	etag, err := s.Store.ReplaceAccountFile(ctx, r.ID, *r.Body, r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	return api.ReplaceAuthFile204Response{Headers: api.ReplaceAuthFile204ResponseHeaders{ETag: etag}}, nil
}
func (s *Server) GetCredentialStores(_ context.Context, _ api.GetCredentialStoresRequestObject) (api.GetCredentialStoresResponseObject, error) {
	items := slices.Sorted(maps.Keys(s.Store.Deployment.CredentialStores))
	if items == nil {
		items = []string{}
	}
	return api.GetCredentialStores200JSONResponse{Items: items}, nil
}
