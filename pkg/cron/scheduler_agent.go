package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// AIAgent keyword arguments the cron run path constructs its agent with
type AgentOptions struct {
	Model                    string
	Runtime                  *provider.Runtime
	MaxIterations            int
	ReasoningConfig          map[string]any
	PrefillMessages          []any
	FallbackModel            []map[string]any
	CredentialPool           *provider.Pool
	ProvidersAllowed         any
	ProvidersIgnored         any
	ProvidersOrder           any
	ProviderSort             any
	OpenRouterMinCodingScore any
	EnabledToolsets          []string
	DisabledToolsets         []string
	QuietMode                bool
	SkipContextFiles         bool
	LoadSoulIdentity         bool
	SkipMemory               bool
	SkipBackgroundReview     bool
	Platform                 string
	SessionID                string
	SessionDB                *state.DB
	Env                      *tools.Env
	Workdir                  string
}

// Live agent of one cron run, implemented by the agent package behind the registered factory
type CronAgent interface {
	RunConversation(ctx context.Context, prompt, taskID string) (map[string]any, error)
	GetActivitySummary() map[string]any
	Interrupt(message string)
	SessionID() string
	SessionAPICalls() int
	SetEndSessionOnClose(v bool)
	Close() error
}

// First truthy value, the last value when none is, like a chain of or
func firstTruthy(vals ...any) any {
	for _, v := range vals {
		if anyutil.IsTrue(v) {
			return v
		}
	}
	if len(vals) == 0 {
		return nil
	}
	return vals[len(vals)-1]
}

// First n code points of s
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Last n code points of s
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
