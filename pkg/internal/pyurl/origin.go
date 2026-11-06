package pyurl

import "strings"

// Scheme, lowercased hostname and effective port of a base URL, empty with port 0 on no host or a bad port
func BaseURLOrigin(baseURL string) (string, string, int, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return "", "", 0, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := Parse(raw)
	if err != nil {
		return "", "", 0, err
	}
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", "", 0, nil
	}
	scheme := strings.ToLower(u.Scheme)
	port, ok, perr := u.Port()
	if perr != nil {
		return "", "", 0, nil
	}
	if !ok {
		port = map[string]int{"https": 443, "http": 80}[scheme]
	}
	return scheme, host, port, nil
}
