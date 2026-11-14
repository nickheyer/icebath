package auth

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
)

type Seed struct {
	Source      string
	AccessToken Secret
	BaseURL     string
	Label       string
}

type RefreshRequest struct {
	Entry     Entry
	Force     bool
	FailedKey Secret
}

type Refresher interface {
	Refresh(ctx context.Context, r RefreshRequest) (Entry, error)
}

type Quarantiner interface {
	Quarantine(ctx context.Context, e Entry, cause error)
}

type SeedSpec struct {
	Provider          string
	Name              string
	AuthType          string
	EnvVars           []string
	BaseURL           string
	BaseURLEnv        string
	ProcessCommand    string
	ProcessArgs       []string
	ProcessCommandEnv []string
	ProcessArgsEnv    string

	ResolveSecret        func(ctx context.Context) (key Secret, source string, ok bool)
	RuntimeBaseURL       func(ctx context.Context, apiKey Secret, def, envURL string) (string, bool)
	RemoveSource         func(source string) (sources, hints []string, ok bool)
	ExternalAuthEvidence func() (verified bool, source string, ok bool)
	Status               func(ctx context.Context) (Status, bool)
	EntitlementMessage   func(ctx context.Context) string
	SeedPool             func(ctx context.Context, suppressed []string, explicit bool) []Seed
	Refresh              Refresher
	RuntimeKey           func(e *Entry) (Secret, bool)
	StateSeedable        func(state json.RawMessage) bool
}

func (s *Store) envFloat(ctx context.Context, name string, def float64) float64 {
	raw := strings.TrimSpace(s.lookupEnv(ctx, name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

func (s *Store) userAgent() string { return "icebath/" + s.version }

func (s *Store) reloginCommand(provider string) string {
	selector := home.ProfileSelector(s.home)
	if strings.ToLower(strings.TrimSpace(provider)) == ProviderPortal {
		return "icebath " + selector + "portal"
	}
	return "icebath " + selector + "auth add " + strings.ToLower(strings.TrimSpace(provider)) + " --type oauth"
}
