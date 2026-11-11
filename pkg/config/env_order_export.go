package config

import (
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Keys of the home .env in first assignment order, the order a load_env mapping iterates
func EnvKeyOrder(h string) []string {
	raw, err := os.ReadFile(home.EnvPath(h))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var keys []string
	for _, line := range strings.Split(DecodeEnvBytes(raw), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimLeft(line[len("export "):], " \t")
		}
		key, _, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if ok && key != "" && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}
