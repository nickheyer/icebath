package gcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	credentialsPathEnv = "VERTEX_CREDENTIALS_PATH"
	defaultPathEnv     = "GOOGLE_APPLICATION_CREDENTIALS"
	projectEnv         = "VERTEX_PROJECT_ID"
	regionEnv          = "VERTEX_REGION"
)

type Options struct {
	Secrets   Env
	Multiplex bool
	Config    func(ctx context.Context) config.VertexConfig
	HTTP      httpclient.Options
}

type cacheKey struct {
	path string
	sum  string
}

type cacheEntry struct {
	creds   *credentials
	project string
}

type Vertex struct {
	secrets   Env
	multiplex bool
	config    func(ctx context.Context) config.VertexConfig
	c         *client

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
}

func NewVertex(o Options) (*Vertex, error) {
	c, err := newClient(o.HTTP)
	if err != nil {
		return nil, err
	}
	secrets := o.Secrets
	if secrets == nil {
		secrets = ProcessEnv
	}
	return &Vertex{secrets: secrets, multiplex: o.Multiplex, config: o.Config, c: c, cache: map[cacheKey]cacheEntry{}}, nil
}

func (v *Vertex) section(ctx context.Context) config.VertexConfig {
	if v.config == nil {
		return config.VertexConfig{}
	}
	return v.config(ctx)
}

func (v *Vertex) Region(ctx context.Context, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if region := strings.TrimSpace(v.secrets(regionEnv)); region != "" {
		return region
	}
	if region := strings.TrimSpace(v.section(ctx).Region); region != "" {
		return region
	}
	return DefaultRegion
}

func (v *Vertex) ProjectOverride(ctx context.Context) string {
	if project := strings.TrimSpace(v.secrets(projectEnv)); project != "" {
		return project
	}
	return strings.TrimSpace(v.section(ctx).ProjectID)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (v *Vertex) resolvePath(explicit string) string {
	if explicit != "" && exists(explicit) {
		return explicit
	}
	for _, name := range []string{credentialsPathEnv, defaultPathEnv} {
		if path := v.secrets(name); path != "" && exists(path) {
			return path
		}
	}
	return ""
}

func snapshot(path string) ([]byte, cacheKey, error) {
	if path == "" {
		return nil, cacheKey{}, nil
	}
	data, err := readBounded(path)
	if err != nil {
		return nil, cacheKey{path: path}, err
	}
	sum := sha256.Sum256(data)
	return data, cacheKey{path: path, sum: hex.EncodeToString(sum[:])}, nil
}

func (v *Vertex) load(ctx context.Context, path string, data []byte, readErr error) (cacheEntry, error) {
	scopes := []string{CloudPlatformScope}
	if path != "" {
		if readErr != nil {
			return cacheEntry{}, &Error{Kind: kindServiceAccount, Message: "key file " + path + " could not be read", Err: readErr}
		}
		info, ok := parseDocument(data)
		if !ok {
			return cacheEntry{}, &Error{Kind: kindServiceAccount, Message: "key file " + path + " is not a valid JSON object"}
		}
		creds, err := v.c.serviceAccount(info, scopes)
		if err != nil {
			return cacheEntry{}, err
		}
		return cacheEntry{creds: creds, project: info.text("project_id")}, nil
	}
	if v.multiplex && os.Getenv(defaultPathEnv) != "" {
		slog.Warn(ErrAmbientRefused.Error())
		return cacheEntry{}, ErrAmbientRefused
	}
	creds, project, err := v.c.defaultCredentials(ctx, scopes)
	if err != nil {
		return cacheEntry{}, err
	}
	return cacheEntry{creds: creds, project: project}, nil
}

func (v *Vertex) cached(ctx context.Context, path string) (cacheEntry, cacheKey, error) {
	data, key, readErr := snapshot(path)
	v.mu.Lock()
	defer v.mu.Unlock()
	if entry, ok := v.cache[key]; ok {
		return entry, key, nil
	}
	entry, err := v.load(ctx, path, data, readErr)
	if err != nil {
		return cacheEntry{}, key, err
	}
	for other := range v.cache {
		if other.path == key.path {
			delete(v.cache, other)
		}
	}
	v.cache[key] = entry
	return entry, key, nil
}

func (v *Vertex) drop(key cacheKey) {
	v.mu.Lock()
	delete(v.cache, key)
	v.mu.Unlock()
}

func (v *Vertex) attempt(ctx context.Context, path string) (auth.Token, string, error) {
	entry, key, err := v.cached(ctx, path)
	if err != nil {
		return auth.Token{}, "", err
	}
	token, expiry, err := entry.creds.ensure(ctx, vertexStale)
	if err != nil {
		v.drop(key)
		return auth.Token{}, "", err
	}
	project := v.ProjectOverride(ctx)
	if project == "" {
		project = entry.project
	}
	return auth.Token{Value: auth.NewSecret(token), Expiry: expiry}, project, nil
}

func (v *Vertex) Credentials(ctx context.Context, credentialsPath string) (auth.Token, string, error) {
	resolved := v.resolvePath(credentialsPath)
	token, project, err := v.attempt(ctx, resolved)
	if err == nil || errors.Is(err, ErrAmbientRefused) {
		return token, project, err
	}
	slog.Error("Failed to resolve Vertex AI credentials", "error", err)
	if resolved == "" {
		if path := v.resolvePath(credentialsPath); path != "" {
			slog.Info("ADC failed, retrying with service account", "path", path)
			return v.Credentials(ctx, path)
		}
	}
	return auth.Token{}, "", err
}

func BaseURL(project, region string) string {
	host := region + "-aiplatform.googleapis.com"
	if region == DefaultRegion {
		host = "aiplatform.googleapis.com"
	}
	return "https://" + host + "/v1beta1/projects/" + project + "/locations/" + region + "/endpoints/openapi"
}

func (v *Vertex) Config(ctx context.Context, credentialsPath, region string) (auth.Token, string, error) {
	token, project, err := v.Credentials(ctx, credentialsPath)
	if err != nil {
		return auth.Token{}, "", err
	}
	if project == "" {
		return auth.Token{}, "", ErrNoProject
	}
	return token, BaseURL(project, v.Region(ctx, region)), nil
}

func (v *Vertex) BaseURL(ctx context.Context, credentialsPath, region string) (string, error) {
	entry, _, err := v.cached(ctx, v.resolvePath(credentialsPath))
	if err != nil {
		if !errors.Is(err, ErrAmbientRefused) {
			slog.Debug("Vertex AI credentials could not be loaded", "error", err)
		}
		return "", err
	}
	project := v.ProjectOverride(ctx)
	if project == "" {
		project = entry.project
	}
	if project == "" {
		return "", ErrNoProject
	}
	return BaseURL(project, v.Region(ctx, region)), nil
}

type tokenSource struct{ v *Vertex }

func (s tokenSource) Token(ctx context.Context) (auth.Token, error) {
	token, _, err := s.v.Credentials(ctx, "")
	if err != nil {
		return auth.Token{}, fmt.Errorf("%w: %w", ErrTokenUnavailable, err)
	}
	return token, nil
}

func (tokenSource) Kind() auth.SourceKind { return auth.SourceVertexOAuth }

func (v *Vertex) TokenSource() auth.TokenSource { return tokenSource{v: v} }

func (v *Vertex) HasCredentials(ctx context.Context) bool {
	return v.resolvePath("") != "" || v.ProjectOverride(ctx) != ""
}

func (v *Vertex) HasExplicitConfig(ctx context.Context) bool {
	if v.ProjectOverride(ctx) != "" {
		return true
	}
	path := v.secrets(credentialsPathEnv)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && fileutil.Accessible(path, fileutil.AccessRead)
}
