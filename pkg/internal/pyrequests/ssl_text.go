package pyrequests

import "strings"

// str of the ssl module exception a failed handshake raises, the strerror its repr carries
func SSLErrorText(err error) string {
	repr := sslErrorRepr(nil, err)
	_, rest, found := strings.Cut(repr, ", ")
	if !found || len(rest) < 3 {
		return repr
	}
	return rest[1 : len(rest)-2]
}
