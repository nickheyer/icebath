package aws

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
)

const (
	DefaultRegion  = "us-east-1"
	PlaceholderKey = "aws-sdk"
	BearerTokenEnv = "AWS_BEARER_TOKEN_BEDROCK"
	RuntimeService = "bedrock-runtime"
	ControlService = "bedrock"
)

var authEnvChain = [][]string{
	{BearerTokenEnv},
	{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"},
	{"AWS_PROFILE"},
	{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"},
	{"AWS_WEB_IDENTITY_TOKEN_FILE"},
}

var runtimeHostRegion = regexp.MustCompile(`bedrock-runtime\.([a-z0-9-]+)\.`)

func BearerToken(env Env) string {
	return strings.TrimSpace(env(BearerTokenEnv))
}

func UsesChain(apiKey string) bool {
	return strings.TrimSpace(apiKey) == "" || apiKey == PlaceholderKey || apiKey == "no-key-required"
}

func EnvSource(env Env) string {
	for _, group := range authEnvChain {
		all := true
		for _, name := range group {
			if strings.TrimSpace(env(name)) == "" {
				all = false
				break
			}
		}
		if all {
			return group[0]
		}
	}
	return ""
}

func Region(env Env) string {
	if v := strings.TrimSpace(env("AWS_REGION")); v != "" {
		return v
	}
	if v := strings.TrimSpace(env("AWS_DEFAULT_REGION")); v != "" {
		return v
	}
	s, err := newSession("")
	if err != nil {
		return DefaultRegion
	}
	if _, err := s.scoped(); err != nil {
		return DefaultRegion
	}
	if v := s.region(); v != "" {
		return v
	}
	return DefaultRegion
}

func RuntimeRegion(cfg config.BedrockConfig, env Env) string {
	if v := strings.TrimSpace(cfg.Region); v != "" {
		return v
	}
	return Region(env)
}

func RegionFromRuntimeURL(baseURL string) string {
	if m := runtimeHostRegion.FindStringSubmatch(baseURL); m != nil {
		return m[1]
	}
	return DefaultRegion
}

func (c *Chain) Endpoint(service, region string) (string, error) {
	s, err := newSession(c.opts.Profile)
	if err != nil {
		return "", err
	}
	return s.endpoint(service, service, region), nil
}
