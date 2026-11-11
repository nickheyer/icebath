package dashboard

import "github.com/nickheyer/icebath/pkg/gateway"

// Normalised X-Forwarded-Prefix of a request, empty when unset or malformed
func PrefixFromRequest(conn *HTTPConnection) string {
	return gateway.NormalisePrefix(conn.Headers().Get("x-forwarded-prefix"))
}
