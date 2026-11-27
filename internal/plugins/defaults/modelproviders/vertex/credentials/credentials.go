package credentials

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	vertexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	// A token this close to its expiry is renewed before it is used
	early = 5 * time.Minute
	// How long a failure to mint a token is believed before trying again
	failedFor = time.Minute
	mintWait  = 20 * time.Second
)

// Google credentials of the Vertex provider: a service account key the account or a variable names, else Application Default Credentials,
// each minting the access tokens of the Vertex OpenAI compatible API
type source struct {
	mu sync.Mutex
	// Credentials by the file and its content they were read from, or adc
	loaded map[string]*google
	// When minting for each failed last, and why
	failed map[string]failure
}

type failure struct {
	at  time.Time
	err error
}

func Register(registry *credentials.Registry) {
	registry.Register(&source{loaded: map[string]*google{}, failed: map[string]failure{}})
}

// Path of the service account key the account uses: its own, else VERTEX_CREDENTIALS_PATH, else GOOGLE_APPLICATION_CREDENTIALS, each only
// when the file is there; empty for Application Default Credentials
func path(account credentials.Account) (string, string) {
	if p := strings.TrimSpace(account.Fields.GetString(vertexconfig.CredentialsPath)); p != "" && account.Name != "" && fsx.Exists(p) {
		return p, ""
	}
	for _, name := range []string{vertexconfig.CredentialsPathEnv, "GOOGLE_APPLICATION_CREDENTIALS"} {
		if p, _ := account.Fields.LookupEnv(name); strings.TrimSpace(p) != "" && fsx.Exists(p) && !account.Ignores(name) {
			return p, name
		}
	}
	return "", ""
}

// Credentials of the key file or of ADC, read once for each content of the file and each profile, whose environment Google's variables are
// read through
func (s *source) credentials(ctx context.Context, account credentials.Account, keyPath string) (*google, string, string, error) {
	scope := account.Fields.Profile() + "|"
	key, raw := scope+"adc", []byte(nil)
	if keyPath != "" {
		var err error
		if raw, err = os.ReadFile(keyPath); err != nil {
			return nil, "", key, err
		}
		sum := sha256.Sum256(raw)
		key = scope + keyPath + "#" + hex.EncodeToString(sum[:8])
	}
	s.mu.Lock()
	cached, ok := s.loaded[key]
	last, failedBefore := s.failed[key]
	s.mu.Unlock()
	if ok {
		return cached, cached.project, key, nil
	}
	if failedBefore && time.Since(last.at) < failedFor {
		return nil, "", key, last.err
	}
	var g *google
	var project string
	var err error
	e := environment{lookup: account.Fields.LookupEnv, all: account.Fields.Environ}
	if keyPath != "" {
		var info map[string]any
		if err = json.Unmarshal(raw, &info); err == nil {
			g, project, err = fromInfo(ctx, e, info, keyPath, []string{cloudPlatform})
		}
	} else {
		g, project, err = adc(ctx, e, []string{cloudPlatform})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failed[key] = failure{at: time.Now(), err: err}
		return nil, "", key, err
	}
	g.project = cmp.Or(g.project, project)
	for other := range s.loaded {
		if strings.HasPrefix(other, scope+keyPath+"#") {
			delete(s.loaded, other)
		}
	}
	s.loaded[key] = g
	delete(s.failed, key)
	return g, g.project, key, nil
}

// Project and region the provider's requests go to: its settings, else the project of its credentials and the global region
func target(account credentials.Account, project string) (string, string) {
	return cmp.Or(strings.TrimSpace(account.Provider.Settings.GetString(vertexconfig.Project)), project),
		cmp.Or(strings.TrimSpace(account.Provider.Settings.GetString(vertexconfig.Region)), vertexconfig.DefaultRegion)
}

// Address of the OpenAI compatible Vertex API of a project and region, the bare host for the global region
func BaseURL(project, region string) string {
	host := region + "-aiplatform.googleapis.com"
	if region == "global" {
		host = "aiplatform.googleapis.com"
	}
	return fmt.Sprintf("https://%s/v1beta1/projects/%s/locations/%s/endpoints/openapi", host, project, region)
}

// Access token minted for the account, minted now when none is or it is about to expire, with the address of its project's API
func (s *source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != vertexconfig.Name {
		return credentials.Credential{}, false
	}
	keyPath, variable := path(account)
	ctx, cancel := context.WithTimeout(context.Background(), mintWait)
	defer cancel()
	g, project, key, err := s.credentials(ctx, account, keyPath)
	if err != nil {
		slog.Debug("Vertex AI credentials could not be loaded", "error", err)
		return credentials.Credential{}, false
	}
	project, region := target(account, project)
	if project == "" {
		slog.Warn("No Vertex AI project id resolves; set the provider's project, VERTEX_PROJECT_ID, or use credentials that embed one")
		return credentials.Credential{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token == "" || time.Until(g.expiry) < early && !g.expiry.IsZero() {
		if err := s.mint(ctx, g, key); err != nil {
			return credentials.Credential{}, false
		}
	}
	credential := credentials.Credential{APIKey: g.token, BaseURL: BaseURL(project, region), Auth: provider.AuthGCPOAuth, Expires: g.expiry,
		Origin: credentials.External, Source: "Google ADC (" + g.kind + ")"}
	switch {
	case variable != "":
		credential.Origin, credential.Source = credentials.Environment, variable
	case keyPath != "":
		credential.Origin, credential.Source = credentials.Saved, ""
	}
	return credential, true
}

// Mints a token, remembering a failure so it isn't tried again for a minute; the caller holds the credential
func (s *source) mint(ctx context.Context, g *google, key string) error {
	s.mu.Lock()
	last, failedBefore := s.failed[key]
	s.mu.Unlock()
	if failedBefore && time.Since(last.at) < failedFor {
		return last.err
	}
	if err := g.refresh(ctx); err != nil {
		slog.Error("Failed to resolve Vertex AI credentials", "error", err)
		s.mu.Lock()
		s.failed[key] = failure{at: time.Now(), err: err}
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	delete(s.failed, key)
	s.mu.Unlock()
	return nil
}

func (s *source) Due(account credentials.Account) bool {
	if account.Spec() != vertexconfig.Name {
		return false
	}
	keyPath, _ := path(account)
	ctx, cancel := context.WithTimeout(context.Background(), mintWait)
	defer cancel()
	g, _, _, err := s.credentials(ctx, account, keyPath)
	if err != nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.token != "" && !g.expiry.IsZero() && time.Until(g.expiry) < early
}

// Mints a new token now, after Vertex refused the last
func (s *source) Renew(ctx context.Context, account credentials.Account) (bool, error) {
	if account.Spec() != vertexconfig.Name {
		return false, nil
	}
	keyPath, _ := path(account)
	g, _, key, err := s.credentials(ctx, account, keyPath)
	if err != nil {
		return true, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s.mu.Lock()
	delete(s.failed, key)
	s.mu.Unlock()
	return true, s.mint(ctx, g, key)
}

// Service account key a file holds, read and checked to be one
type serviceKey struct {
	Type        string `json:"type"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	ProjectID   string `json:"project_id"`
}

// Sign-in of a Vertex account with a Google service account key, whose path the account keeps
var ServiceAccount = credentials.SignIn{Spec: vertexconfig.Name, Title: "Google service account key", Type: credentials.APIKey, Run: serviceAccountKey}

func serviceAccountKey(p *credentials.Prompter) (credentials.Signed, error) {
	keyPath := p.Option("key")
	if keyPath == "" {
		typed, ok := p.Ask("Service account key file, a JSON file: ", false)
		if !ok {
			return credentials.Signed{}, credentials.ErrCancelled
		}
		keyPath = strings.TrimSpace(typed)
	}
	if keyPath == "" {
		return credentials.Signed{}, errors.New("No key file was given, so nothing was added.")
	}
	if strings.HasPrefix(keyPath, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			keyPath = filepath.Join(home, strings.TrimPrefix(keyPath, "~"))
		}
	}
	if absolute, err := filepath.Abs(keyPath); err == nil {
		keyPath = absolute
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return credentials.Signed{}, fmt.Errorf("The key file %s can't be read: %v.", keyPath, err)
	}
	var key serviceKey
	if json.Unmarshal(raw, &key) != nil || key.Type != "service_account" || key.ClientEmail == "" || key.PrivateKey == "" {
		return credentials.Signed{}, fmt.Errorf("%s is not a Google service account key, which holds \"type\": \"service_account\", client_email "+
			"and private_key. Make one in the Google Cloud console under IAM & Admin › Service accounts › Keys.", keyPath)
	}
	p.Say("✓ Saved the service account %s of the project %s as this account's key.", key.ClientEmail, key.ProjectID)
	return credentials.Signed{Fields: map[string]any{vertexconfig.CredentialsPath: keyPath}, Identity: key.ClientEmail}, nil
}

// Why the account has no credential: the Google credentials that couldn't be loaded or minted, or the project none names
func (s *source) Why(account credentials.Account) (string, bool) {
	if account.Spec() != vertexconfig.Name {
		return "", false
	}
	keyPath, _ := path(account)
	ctx, cancel := context.WithTimeout(context.Background(), mintWait)
	defer cancel()
	_, project, key, err := s.credentials(ctx, account, keyPath)
	if err != nil {
		return fmt.Sprintf("No Google sign-in can be used: %v. Run `gcloud auth application-default login`, set GOOGLE_APPLICATION_CREDENTIALS, "+
			"or add a service account key with `icebath auth add %s`.", strings.TrimSuffix(err.Error(), "."), account.Provider.ID), true
	}
	if project, _ = target(account, project); project == "" {
		return fmt.Sprintf("No Google Cloud project is known for Vertex AI. Set the project in /providers or set %s, or use credentials "+
			"that name one.", vertexconfig.ProjectEnv), true
	}
	s.mu.Lock()
	last, failed := s.failed[key]
	s.mu.Unlock()
	if failed {
		return "Google did not give a token for Vertex AI: " + strings.TrimSuffix(last.err.Error(), ".") + ".", true
	}
	return "", false
}
