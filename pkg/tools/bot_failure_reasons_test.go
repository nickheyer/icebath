package tools_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	fixtureAnthropic401 = "Error code: 401 - {'type': 'error', 'error': {'type': 'authentication_error', " +
		"'message': 'Your API key is invalid, blocked or out of funds...'}}"
	fixtureNoProvider = "agent init failed: No LLM provider configured. Run `icebath model` to select " +
		"a provider, or run `icebath setup` for first-time configuration."
	fixtureNoToken     = "agent init failed: No access token found for Nous Portal login."
	fixtureTargetScope = "Icebath could not read this profile's API key (an internal profile-scoping bug on the " +
		"multiplexed gateway, not your configuration). Run `icebath gateway restart`; if it keeps " +
		"happening, report it with `icebath debug share`."
	fixtureTargetScopeDetailed = "served_profile_child_env(inherit_credentials=True) called with no target home and no profile " +
		"secret scope bound while multiplexing is on; the child would inherit the launch profile's " +
		"credentials. Bind the profile scope (or pass target_home) at the spawn site."
)

func TestClassifyAgentErrorRules(t *testing.T) {
	for _, tc := range []struct {
		text any
		code string
	}{
		{"Error code: 403 - forbidden", tools.ReasonProviderAuthOrAccess},
		{"Invalid API key provided", tools.ReasonProviderAuthOrAccess},
		{"Error code: 402 - payment required", tools.ReasonProviderQuotaLimit},
		{"insufficient balance, top up your account", tools.ReasonProviderQuotaLimit},
		{"You exceeded your current quota", tools.ReasonProviderQuotaLimit},
		{"Error code: 429 - Too Many Requests", tools.ReasonProviderRateLimit},
		{"Rate limit reached for gpt-4o", tools.ReasonProviderRateLimit},
		{"Error code: 500 - internal server error", tools.ReasonProviderServerError},
		{"Error code: 529 - overloaded_error: Overloaded", tools.ReasonProviderServerError},
		{"This model's maximum context length is 128000 tokens", tools.ReasonContextOverflow},
		{"context_overflow: prompt too large", tools.ReasonContextOverflow},
		{"missing config: no provider block in config.yaml", tools.ReasonMissingConfig},
		{"model 'gpt-9' not found", tools.ReasonModelUnavailable},
		{"The model `foo-bar` does not exist", tools.ReasonModelUnavailable},
		{"model_not_found", tools.ReasonModelUnavailable},
		{"status: 401 unauthorized", tools.ReasonProviderAuthOrAccess},
		{"upstream server error", tools.ReasonProviderServerError},
		{fixtureTargetScope, tools.ReasonTargetScopeUnresolved},
		{fixtureTargetScopeDetailed, tools.ReasonTargetScopeUnresolved},
		{provider.NewUnscopedSecretError("OPENROUTER_API_KEY", "").Error(), tools.ReasonTargetScopeUnresolved},
		{"gate check failed: error at line 502 of module", tools.ReasonUnknown},
		{"took 429 ms to fail", tools.ReasonUnknown},
		{"something inexplicable happened", tools.ReasonUnknown},
		{"", tools.ReasonUnknown},
		{nil, tools.ReasonUnknown},
	} {
		if got := tools.ClassifyAgentError(tc.text); got != tc.code {
			t.Errorf("%q: %s want %s", tc.text, got, tc.code)
		}
	}
}

func TestFixtureAnthropic401AuthBeatsQuota(t *testing.T) {
	if !strings.Contains(fixtureAnthropic401, "out of funds") || tools.ClassifyAgentError(fixtureAnthropic401) != tools.ReasonProviderAuthOrAccess {
		t.Fatal(tools.ClassifyAgentError(fixtureAnthropic401))
	}
}

func TestPrecedenceAuthenticationErrorTypeAloneBeatsQuotaWords(t *testing.T) {
	if got := tools.ClassifyAgentError("authentication_error: account is out of funds"); got != tools.ReasonProviderAuthOrAccess {
		t.Fatal(got)
	}
	if got := tools.ClassifyAgentError("account is out of funds"); got != tools.ReasonProviderQuotaLimit {
		t.Fatal(got)
	}
}

func TestFixtureNoProviderConfiguredIsMissingConfig(t *testing.T) {
	if got := tools.ClassifyAgentError(fixtureNoProvider); got != tools.ReasonMissingConfig {
		t.Fatal(got)
	}
}

func TestFixtureNoAccessTokenIsMissingConfig(t *testing.T) {
	if got := tools.ClassifyAgentError(fixtureNoToken); got != tools.ReasonMissingConfig {
		t.Fatal(got)
	}
}

func TestAutoRetryableSetAndPredicate(t *testing.T) {
	for code := range tools.AutoRetryable {
		if !tools.IsAutoRetryable(code) {
			t.Fatal(code)
		}
	}
	for code := range tools.AllReasons {
		if !tools.AutoRetryable[code] && tools.IsAutoRetryable(code) {
			t.Fatal(code)
		}
	}
	if tools.IsAutoRetryable("") || tools.IsAutoRetryable("nonsense") {
		t.Fatal("blank or unknown reason retried")
	}
}

func TestTransientReasonsResume(t *testing.T) {
	reasons := make([]string, 0, len(tools.AutoRetryable))
	for reason := range tools.AutoRetryable {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		if tools.RetryAction(reason) != tools.RetryResume {
			t.Fatal(reason)
		}
	}
}

func TestContextOverflowCompressesThenResumes(t *testing.T) {
	if tools.RetryAction(tools.ReasonContextOverflow) != tools.RetryCompressThenResume {
		t.Fatal(tools.RetryAction(tools.ReasonContextOverflow))
	}
}

func TestNonRetryableReasonsStop(t *testing.T) {
	for _, reason := range []string{tools.ReasonProviderAuthOrAccess, tools.ReasonProviderQuotaLimit, tools.ReasonMissingConfig,
		tools.ReasonModelUnavailable, tools.ReasonAgentBlocked, tools.ReasonCancelled, tools.ReasonQueuedExpired, tools.ReasonUnknown, "", "not-a-reason"} {
		if tools.RetryAction(reason) != tools.RetryNone {
			t.Fatal(reason)
		}
	}
}
