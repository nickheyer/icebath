package profiles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Issues a GET through the provider and decodes the JSON body into v
func GetJSON(ctx context.Context, p *provider.Provider, path string, query url.Values, v any) error {
	return DoJSON(ctx, p, protocol.Call{Method: http.MethodGet, Path: path, Query: query, Header: http.Header{"Accept": {"application/json"}}}, v)
}

// Issues any call through the provider and decodes the JSON body into v
func DoJSON(ctx context.Context, p *provider.Provider, call protocol.Call, v any) error {
	resp, err := p.Do(ctx, call)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s %s: %w", call.Method, call.Path, err)
	}
	return nil
}
