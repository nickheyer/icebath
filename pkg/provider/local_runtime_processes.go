package provider

import "strings"

var credentialEnvMarkers = []string{"_API_KEY", "_TOKEN", "_SECRET", "PASSWORD", "_CREDENTIALS"}

// Returns a copy of base without any variable whose uppercased name carries a credential marker
func ServerChildEnv(base map[string]string) map[string]string {
	out := make(map[string]string, len(base))
	for key, value := range base {
		upper := strings.ToUpper(key)
		credential := false
		for _, marker := range credentialEnvMarkers {
			if strings.Contains(upper, marker) {
				credential = true
				break
			}
		}
		if !credential {
			out[key] = value
		}
	}
	return out
}
