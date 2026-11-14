package config

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Characters a displayed secret never carries, controls, zero width marks and direction overrides
var displayControl = regexp.MustCompile(`[\x{00}-\x{1f}\x{7f}\x{80}-\x{9f}\x{200b}-\x{200f}\x{202a}-\x{202e}\x{2060}-\x{2064}]`)

// Config value holding nothing but one environment reference, which names no secret itself
var envReferenceOnly = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}\n?$`)

// Key names whose value is a credential, matched whole so names such as token_count stay visible
func secretConfigKeys() []string {
	return []string{"api_key", "apikey", "key", "token", "access_token", "refresh_token", "id_token",
		"secret", "client_secret", "password", "passwd", "authorization", "private_key", "bearer", "jwt"}
}

// Key name endings whose value is a credential
func secretConfigSuffixes() []string {
	return []string{"_api_key", "_token", "_secret", "_password", "_key", "_access_key"}
}

// Endings that name the settings among the keys routed to the env file that are no secrets
func nonSecretSuffixes() []string {
	return []string{"_url", "_host", "_user", "_id", "_domain", "_scheme"}
}

// Text shown in place of an API key that is not set
const KeyNotSet = "(not set)"

// Secret shortened for display to its first and last four characters, three stars when shorter than twelve, empty when unset
func MaskSecret(value string) string {
	value = displayControl.ReplaceAllString(value, "")
	if value == "" {
		return ""
	}
	if utf8.RuneCountInString(value) < 12 {
		return "***"
	}
	runes := []rune(value)
	return string(runes[:4]) + "..." + string(runes[len(runes)-4:])
}

// API key shortened for display, the not set text when it is empty
func RedactKey(key string) string {
	if masked := MaskSecret(key); masked != "" {
		return masked
	}
	return KeyNotSet
}

// Reports a config key whose last segment names a credential value, header names read in snake case
func IsSecretConfigKey(key string) bool {
	leaf := key
	if at := strings.LastIndex(key, "."); at >= 0 {
		leaf = key[at+1:]
	}
	leaf = strings.ReplaceAll(strings.ToLower(leaf), "-", "_")
	hasSuffix := func(suffixes []string) bool {
		return slices.ContainsFunc(suffixes, func(s string) bool { return strings.HasSuffix(leaf, s) })
	}
	if IsEnvConfigKey(key) {
		return !hasSuffix(nonSecretSuffixes())
	}
	return slices.Contains(secretConfigKeys(), leaf) || hasSuffix(secretConfigSuffixes())
}

// Copy of a config value tree with the text under every credential shaped key masked, environment references kept
func RedactConfigValue(n *yaml.Node) *yaml.Node {
	return redactTree(plainCopy(n), 0)
}

// Masks credential values in place below n, descending at most twenty levels
func redactTree(n *yaml.Node, depth int) *yaml.Node {
	if n == nil || depth > 20 {
		return n
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if isStringNode(k) && IsSecretConfigKey(k.Value) && isStringNode(v) && v.Value != "" && !envReferenceOnly.MatchString(v.Value) {
				n.Content[i+1] = strNode(MaskSecret(v.Value))
				continue
			}
			redactTree(v, depth+1)
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			redactTree(item, depth+1)
		}
	}
	return n
}
