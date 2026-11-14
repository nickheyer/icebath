package providers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	defaultListTimeout = 8 * time.Second
	listBodyLimit      = 32 << 20
)

type Net struct {
	Client  *http.Client
	Version string
}

func (n Net) UserAgent() string {
	if n.Version == "" {
		return "icebath-cli"
	}
	return "icebath-cli/" + n.Version
}

func listTimeout(in ListInput) time.Duration {
	if in.Timeout > 0 {
		return in.Timeout
	}
	return defaultListTimeout
}

func (n Net) getJSON(ctx context.Context, rawURL string, timeout time.Duration, header http.Header) (wire.Value, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return wire.Value{}, err
	}
	req.Header = header
	resp, err := n.Client.Do(req)
	if err != nil {
		return wire.Value{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, listBodyLimit))
	if err != nil {
		return wire.Value{}, err
	}
	if resp.StatusCode >= 400 {
		return wire.Value{}, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return wire.ParseValue(body)
}

func catalogItems(v wire.Value) []wire.Value {
	if v.IsArray() {
		return v.Elems()
	}
	return v.Get("data").Elems()
}

func idsOf(items []wire.Value) []string {
	ids := []string{}
	for _, item := range items {
		if id := item.Get("id"); item.IsObject() && id.Exists() {
			ids = append(ids, id.Display())
		}
	}
	return ids
}

func FetchModels(ctx context.Context, n Net, s *Spec, in ListInput) ([]string, bool) {
	if s.SkipModelListing {
		return nil, false
	}
	callerBase := strings.TrimSpace(in.BaseURL)
	effective := callerBase
	if effective == "" {
		effective = s.BaseURL
	}
	custom := callerBase != "" && strings.TrimRight(callerBase, "/") != strings.TrimRight(s.BaseURL, "/")
	var target string
	if custom {
		target = strings.TrimRight(callerBase, "/") + "/models"
	} else {
		target = strings.TrimSpace(s.ModelsURL)
		if target == "" {
			if effective == "" {
				return nil, false
			}
			target = strings.TrimRight(effective, "/") + "/models"
		}
	}
	header := http.Header{}
	if key := in.APIKey.Reveal(); key != "" {
		header.Set("Authorization", "Bearer "+key)
	}
	header.Set("Accept", "application/json")
	header.Set("User-Agent", n.UserAgent())
	for k, v := range s.DefaultHeaders {
		header.Set(k, v)
	}
	data, err := n.getJSON(ctx, target, listTimeout(in), header)
	if err != nil {
		slog.Debug(fmt.Sprintf("fetch_models(%s): %v", s.Name, err))
		return nil, false
	}
	if in.ChatIDs == nil {
		slog.Debug(fmt.Sprintf("fetch_models(%s): the caller passed no chat catalog filter", s.Name))
		return nil, false
	}
	return in.ChatIDs(catalogItems(data)), true
}

func ListModels(ctx context.Context, n Net, p Profile, in ListInput) ([]string, bool) {
	if l, ok := p.(ModelLister); ok {
		return l.ListModels(ctx, in)
	}
	return FetchModels(ctx, n, p.Spec(), in)
}
