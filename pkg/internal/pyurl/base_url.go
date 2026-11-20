package pyurl

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Parses a base URL like urlparse with a bare host, port and path read as a netloc, nil when blank
func parseBaseURL(baseURL string) (*URL, error) {
	raw := pystr.Strip(baseURL)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Lowercased hostname of a parsed base URL with trailing dots removed, empty without a parse
func hostnameOf(u *URL) string {
	if u == nil {
		return ""
	}
	return strings.TrimRight(pystr.Lower(u.Hostname()), ".")
}

// Lowercased path of a base URL without trailing slashes, empty for a bare host, the urlparse ValueError on a bad netloc
func BaseURLPath(baseURL string) (string, error) {
	u, err := parseBaseURL(baseURL)
	if err != nil || u == nil {
		return "", err
	}
	return strings.TrimRight(pystr.Lower(u.Path), "/"), nil
}
