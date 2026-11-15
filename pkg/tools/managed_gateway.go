package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	defaultToolGatewayDomain = "nousresearch.com"
	defaultToolGatewayScheme = "https"
)

// ManagedToolGatewayConfig describes a resolved Nous hosted vendor passthrough
type ManagedToolGatewayConfig struct {
	Vendor        string
	GatewayOrigin string
	NousUserToken string
	ManagedMode   bool
}

// TOOL_GATEWAY_USER_TOKEN override through the bound secret scope, the process env only for an unscoped multiplex read
func readUserTokenOverride(ctx context.Context, env *Env) string {
	value, err := secretThroughScope(ctx, env, "TOOL_GATEWAY_USER_TOKEN")
	if errors.Is(err, ErrUnscopedSecret) {
		return cleanGatewayValue(os.Getenv("TOOL_GATEWAY_USER_TOKEN"))
	}
	if value == nil {
		return ""
	}
	return cleanGatewayValue(*value)
}

// Value stripped when it is a non blank string, empty for a blank string or any other value
func cleanGatewayValue(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return pystr.Strip(s)
}

// Cheap token probe with no expiry check and no network, reading the override through the env's own secrets
func peekNousAccessToken(env *Env) string {
	if v := readUserTokenOverride(context.Background(), env); v != "" {
		return v
	}
	if NousAccount == nil {
		return ""
	}
	return cleanGatewayValue(NousAccount.PeekAccessToken(env))
}

// Stored Nous access token read without a refresh, empty when none
func PeekNousAccessToken(env *Env) string { return peekNousAccessToken(env) }

// Nous Subscriber access token of the context's profile refreshed through the account source, empty with no identity
func ReadNousAccessToken(ctx context.Context, env *Env) string {
	return readScopedNousAccessToken(ctx, env)
}

// Nous Subscriber access token with the override read through the env's own secrets, the reader managed gateways take
func readNousAccessToken(env *Env) string {
	return readScopedNousAccessToken(context.Background(), env)
}

// Reads a Nous Subscriber access token, the override through the call's bound scope, refreshing when needed
func readScopedNousAccessToken(ctx context.Context, env *Env) string {
	if v := readUserTokenOverride(ctx, env); v != "" {
		return v
	}
	if NousAccount == nil {
		return ""
	}
	return cleanGatewayValue(NousAccount.ReadAccessToken(env))
}

// ToolGatewayScheme returns the configured shared gateway URL scheme
func ToolGatewayScheme() (string, error) {
	scheme := strings.ToLower(strings.TrimSpace(os.Getenv("TOOL_GATEWAY_SCHEME")))
	if scheme == "" {
		scheme = defaultToolGatewayScheme
	}
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("TOOL_GATEWAY_SCHEME must be 'http' or 'https'")
	}
	return scheme, nil
}

// BuildVendorGatewayURL returns the gateway origin for a vendor
func BuildVendorGatewayURL(vendor string) (string, error) {
	envKey := strings.ToUpper(strings.ReplaceAll(vendor, "-", "_")) + "_GATEWAY_URL"
	if explicit := strings.TrimRight(strings.TrimSpace(os.Getenv(envKey)), "/"); explicit != "" {
		return explicit, nil
	}
	domain := strings.Trim(strings.TrimSpace(os.Getenv("TOOL_GATEWAY_DOMAIN")), "/")
	if domain == "" {
		domain = defaultToolGatewayDomain
	}
	scheme, err := ToolGatewayScheme()
	if err != nil {
		return "", err
	}
	return scheme + "://" + vendor + "-gateway." + domain, nil
}

func vendorGateway(env *Env, vendor string, builder func(string) (string, error), tokenReader func(*Env) string) (*ManagedToolGatewayConfig, error) {
	if builder == nil {
		builder = BuildVendorGatewayURL
	}
	if tokenReader == nil {
		tokenReader = readNousAccessToken
	}
	origin, err := builder(vendor)
	if err != nil {
		return nil, err
	}
	token := tokenReader(env)
	if origin == "" || token == "" {
		return nil, nil
	}
	return &ManagedToolGatewayConfig{Vendor: vendor, GatewayOrigin: origin, NousUserToken: token, ManagedMode: true}, nil
}

// ResolveManagedToolGateway resolves the shared gateway config for entitled accounts only
func ResolveManagedToolGateway(env *Env, vendor string, builder func(string) (string, error), tokenReader func(*Env) string) (*ManagedToolGatewayConfig, error) {
	if !managedNousToolsEnabled(env, false) {
		return nil, nil
	}
	return vendorGateway(env, vendor, builder, tokenReader)
}

// ResolveFreeSearchGateway resolves the Perplexity fast search route any Nous identity may use
func ResolveFreeSearchGateway(env *Env, tokenReader func(*Env) string) (*ManagedToolGatewayConfig, error) {
	return vendorGateway(env, "perplexity", nil, tokenReader)
}

// IsManagedToolGatewayReady reports whether a gateway URL and a likely usable token are present
func IsManagedToolGatewayReady(env *Env, vendor string, builder func(string) (string, error), tokenReader func(*Env) string) bool {
	if tokenReader == nil {
		tokenReader = peekNousAccessToken
	}
	cfg, err := ResolveManagedToolGateway(env, vendor, builder, tokenReader)
	return err == nil && cfg != nil
}
