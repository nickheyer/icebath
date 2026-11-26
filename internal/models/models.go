package models

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
)

const UserAgent = "icebath-cli/dev"

type Registrant interface {
	Models(ctx context.Context, route provider.Route) ([]string, bool)
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	for _, registrant := range r.registrants {
		if ids, ok := registrant.Models(ctx, route); ok {
			return ids, true
		}
	}
	return nil, false
}

type Listed struct {
	Provider string
	URL      string
	Public   bool
}

func (l Listed) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != l.Provider {
		return nil, false
	}
	if l.Public {
		route.APIKey = ""
	}
	return List(ctx, route, l.URL)
}

type Models struct {
	Registry
}

func New() *Models {
	return &Models{}
}

func List(ctx context.Context, route provider.Route, modelsURL string) ([]string, bool) {
	target := strings.TrimSpace(modelsURL)
	if target == "" {
		if route.BaseURL == "" {
			return nil, false
		}
		target = strings.TrimRight(route.BaseURL, "/") + "/models"
	}
	headers := map[string]string{"Accept": "application/json", "User-Agent": UserAgent}
	if route.APIKey != "" {
		headers["Authorization"] = "Bearer " + route.APIKey
	}
	maps.Copy(headers, route.Headers)
	data, err := GetJSON(ctx, target, 8*time.Second, headers)
	if err != nil {
		return nil, false
	}
	items := data
	if document, ok := data.(map[string]any); ok {
		items = document["data"]
	} else if _, ok := data.([]any); !ok {
		return nil, false
	}
	return ChatCatalogIDs(items), true
}

func GetJSON(ctx context.Context, target string, timeout time.Duration, headers map[string]string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		request.Header.Set(name, headers[name])
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", target, response.Status)
	}
	if strings.EqualFold(response.Header.Get("Content-Encoding"), "gzip") {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if body, err = io.ReadAll(reader); err != nil {
			return nil, err
		}
	}
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return data, nil
}
