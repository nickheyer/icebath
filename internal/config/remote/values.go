package remote

import (
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	secretKeys = []string{"api_key", "apikey", "key", "token", "access_token", "refresh_token", "id_token", "secret",
		"client_secret", "password", "passwd", "authorization", "private_key", "bearer", "jwt"}
	secretKeySuffixes    = []string{"_api_key", "_token", "_secret", "_password", "_key", "_access_key"}
	nonSecretKeySuffixes = []string{"_url", "_host", "_user", "_id", "_domain", "_scheme"}
	envConfigKeys        = []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "VOICE_TOOLS_OPENAI_KEY",
		"EXA_API_KEY", "PARALLEL_API_KEY", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "FIRECRAWL_GATEWAY_URL",
		"TOOL_GATEWAY_URL", "CONNECTOR_GATEWAY_URL", "TOOL_GATEWAY_DOMAIN", "TOOL_GATEWAY_SCHEME",
		"TOOL_GATEWAY_USER_TOKEN", "TAVILY_API_KEY", "PERPLEXITY_API_KEY", "API_SERVER_KEY", "BROWSERBASE_API_KEY",
		"BROWSERBASE_PROJECT_ID", "BROWSER_USE_API_KEY", "FAL_KEY", "TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN",
		"TERMINAL_SSH_HOST", "TERMINAL_SSH_USER", "TERMINAL_SSH_KEY", "SUDO_PASSWORD", "SLACK_BOT_TOKEN",
		"SLACK_APP_TOKEN", "GITHUB_TOKEN", "HONCHO_API_KEY"}
	envConfigKeySuffixes = []string{"_API_KEY", "_TOKEN", "_SECRET"}
	envConfigKeyPrefixes = []string{"TERMINAL_SSH"}
	secretPlaceholder    = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
	nonSecretPaths       = [][]string{{"voice", "record_key"}, {"terminal", "docker_shared_container_key"},
		{"browser", "camofox", "session_key"}}
)

func hasAnySuffix(s string, suffixes []string) bool {
	return slices.ContainsFunc(suffixes, func(x string) bool { return strings.HasSuffix(s, x) })
}

func isEnvConfigKey(key string) bool {
	if strings.Contains(key, ".") {
		return false
	}
	up := strings.ToUpper(key)
	return slices.Contains(envConfigKeys, up) || hasAnySuffix(up, envConfigKeySuffixes) ||
		slices.ContainsFunc(envConfigKeyPrefixes, func(p string) bool { return strings.HasPrefix(up, p) })
}

func isSecretPath(segments []string) bool {
	if slices.ContainsFunc(nonSecretPaths, func(p []string) bool { return samePath(p, segments) }) {
		return false
	}
	last := segments[len(segments)-1]
	leaf := strings.ReplaceAll(strings.ToLower(last), "-", "_")
	if len(segments) == 1 && isEnvConfigKey(last) {
		return !hasAnySuffix(leaf, nonSecretKeySuffixes)
	}
	return slices.Contains(secretKeys, leaf) || hasAnySuffix(leaf, secretKeySuffixes)
}

func secretLiteralPath(path []string, value *yaml.Node) ([]string, bool) {
	var found []string
	var walk func(p []string, n *yaml.Node) bool
	walk = func(p []string, n *yaml.Node) bool {
		if len(p) > 0 && isSecretPath(p) && n != nil && n.Kind == yaml.ScalarNode {
			switch n.Tag {
			case "!!str":
				if n.Value != "" && !secretPlaceholder.MatchString(n.Value) {
					found = p
					return true
				}
			case "!!int", "!!float":
				found = p
				return true
			}
		}
		if n == nil {
			return false
		}
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				if walk(extend(p, n.Content[i].Value), n.Content[i+1]) {
					return true
				}
			}
		case yaml.SequenceNode:
			for _, it := range n.Content {
				if walk(p, it) {
					return true
				}
			}
		}
		return false
	}
	return found, walk(path, value)
}
