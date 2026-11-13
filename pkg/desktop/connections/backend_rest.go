package connections

import (
	"context"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Method, body, upload, timeout and profile scoping of one REST call against a resolved descriptor
type backendFetchOptions struct {
	Method        string
	Body          any
	HasBody       bool
	Upload        *Upload
	TimeoutMs     float64
	ProfileScoped bool
}

// Upload of a renderer api request, its bytes sent base64 encoded by the bridge
func uploadOf(v any) *Upload {
	obj, ok := v.(map[string]any)
	if !ok || !jsTruthy(v) {
		return nil
	}
	u := &Upload{Filename: jsOrString(obj["filename"]), ContentType: jsOrString(obj["contentType"])}
	if text, ok := obj["bytes"].(string); ok {
		u.Bytes = nodeBase64Decode(text)
	}
	return u
}

// REST call against a descriptor, a native grant assembling all profile reads from one read per profile
func (s *Service) fetchJSONForBackend(ctx context.Context, descriptor backend.Connection, path string, o backendFetchOptions) (any, error) {
	if descriptor.Endpoint() != nil && !o.ProfileScoped {
		sessions := IsAllProfilesSessionListRequest(o.Method, path)
		if sessions || IsAllProfilesAggregateRequest(o.Method, path) {
			listing, err := s.fetchJSONForBackend(ctx, descriptor, "/api/profiles", backendFetchOptions{TimeoutMs: o.TimeoutMs})
			if err != nil {
				return nil, err
			}
			profiles := []string{}
			if list, ok := jsObject(listing)["profiles"].([]any); ok {
				for _, p := range list {
					if name, ok := jsObject(p)["name"].(string); ok && name != "" {
						profiles = append(profiles, name)
					}
				}
			}
			readProfile := func(_ string, scoped string) (any, error) {
				return s.fetchJSONForBackend(ctx, descriptor, scoped, backendFetchOptions{Method: o.Method, Body: o.Body, HasBody: o.HasBody,
					TimeoutMs: o.TimeoutMs, ProfileScoped: true})
			}
			if sessions {
				return FetchAllProfilesSessionsPerProfile(path, profiles, readProfile)
			}
			return FetchAllProfilesAggregatePerProfile(path, profiles, readProfile)
		}
	}
	return s.requestDescriptorJSON(ctx, descriptor, path, JSONOptions{Method: o.Method, Body: o.Body, HasBody: o.HasBody, Upload: o.Upload, TimeoutMs: o.TimeoutMs})
}
