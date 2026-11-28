package web

import (
	"cmp"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Why the website blocklist refuses a URL
type Block struct {
	URL  string
	Host string
	Rule string
	// Where the rule comes from: config, or the file that holds it
	Source  string
	Message string
}

type rule struct {
	pattern, source string
}

// Rule of the blocklist refusing the URL, nil when it is allowed or the blocklist is off. A shared file that cannot be read adds no rules
func Blocked(cfg *config.Config, raw string) *Block {
	if !cfg.GetBool(webconfig.BlocklistEnabled) {
		return nil
	}
	host := urlHost(raw)
	if host == "" {
		return nil
	}
	for _, rule := range rules(cfg) {
		if matches(host, rule.pattern) {
			return &Block{URL: raw, Host: host, Rule: rule.pattern, Source: rule.source,
				Message: fmt.Sprintf("Blocked by website policy: '%s' matched rule '%s' from %s", host, rule.pattern, rule.source)}
		}
	}
	return nil
}

// Rules of the settings, then of each shared file, each once
func rules(cfg *config.Config) []rule {
	var out []rule
	add := func(pattern, source string) {
		if pattern = siteOf(pattern); pattern != "" && !slices.Contains(out, rule{pattern, source}) {
			out = append(out, rule{pattern, source})
		}
	}
	for _, domain := range cfg.GetStringSlice(webconfig.BlocklistDomains) {
		add(domain, "config")
	}
	for _, file := range cfg.GetStringSlice(webconfig.BlocklistSharedFiles) {
		if file = strings.TrimSpace(file); file == "" {
			continue
		}
		file = fsx.ExpandHome(file)
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(strings.TrimPrefix(string(data), "\uFEFF"), "\n") {
			add(strings.TrimRight(line, "\r"), file)
		}
	}
	return out
}

// Site a rule names, a host, a URL, or a host and path, as a lower case host without www. Empty for a blank line or a comment
func siteOf(entry string) string {
	value := strings.ToLower(strings.TrimSpace(entry))
	if value == "" || strings.HasPrefix(value, "#") {
		return ""
	}
	if strings.Contains(value, "://") {
		_, netloc, rest, _, _ := cut(value)
		value = cmp.Or(netloc, rest)
	}
	host, _, _ := strings.Cut(value, "/")
	return strings.TrimPrefix(strings.TrimRight(strings.TrimSpace(host), "."), "www.")
}

// Whether a host falls under a rule: a *. rule as a glob, a site and every subdomain of it otherwise
func matches(host, pattern string) bool {
	if strings.HasPrefix(pattern, "*.") {
		ok, err := path.Match(pattern, host)
		return err == nil && ok
	}
	return host == pattern || strings.HasSuffix(host, "."+pattern)
}

// Host of a URL, or of a bare host and path
func urlHost(raw string) string {
	host, _ := hostOf(raw)
	if host == "" && !strings.Contains(raw, "://") {
		host, _ = hostOf("//" + raw)
	}
	return bare(host)
}
