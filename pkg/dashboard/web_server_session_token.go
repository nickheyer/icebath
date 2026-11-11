package dashboard

import (
	"crypto/rand"
	"encoding/base64"
)

// Fresh url safe token of n random bytes, the secrets.token_urlsafe shape
func tokenURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
