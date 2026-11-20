package pyurl

import "github.com/nickheyer/icebath/pkg/internal/pystr"

// Scheme, lowercased hostname and effective port of a base URL, empty with port 0 on no host or a bad port
func BaseURLOrigin(baseURL string) (string, string, int, error) {
	u, err := parseBaseURL(baseURL)
	if err != nil {
		return "", "", 0, err
	}
	host := hostnameOf(u)
	if host == "" {
		return "", "", 0, nil
	}
	scheme := pystr.Lower(u.Scheme)
	port, ok, perr := u.Port()
	if perr != nil {
		return "", "", 0, nil
	}
	if !ok {
		port = map[string]int{"https": 443, "http": 80}[scheme]
	}
	return scheme, host, port, nil
}
