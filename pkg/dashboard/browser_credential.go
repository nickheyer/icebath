package dashboard

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var (
	browserRevokersMu sync.Mutex
	browserRevokers   = map[string]func() int{}
	browserRevokerSeq []string
)

// Browser credential in use now, the dashboard session token
func BrowserCredentialCurrent() string { return SessionToken() }

// Generation a grant made under the credential records, its SHA-256 hex digest
func BrowserCredentialGenerationOf(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])
}

// Generation of the credential in use now
func BrowserCredentialGeneration() string {
	return BrowserCredentialGenerationOf(BrowserCredentialCurrent())
}

// Whether a grant that recorded the generation was made under the credential in use now
func BrowserCredentialIsCurrent(recorded *string) bool {
	_, ok := BrowserCredentialCurrentIf(recorded)
	return ok
}

// Credential in use now when the recorded generation is its own, read once so a rotation cannot slip between
func BrowserCredentialCurrentIf(recorded *string) (string, bool) {
	credential := BrowserCredentialCurrent()
	if recorded == nil || subtle.ConstantTimeCompare([]byte(*recorded), []byte(BrowserCredentialGenerationOf(credential))) != 1 {
		return "", false
	}
	return credential, true
}

// Runs the revoker each time the credential is replaced, a repeated name replacing the earlier one
func BrowserCredentialOnRotate(name string, revoker func() int) {
	browserRevokersMu.Lock()
	defer browserRevokersMu.Unlock()
	if _, ok := browserRevokers[name]; !ok {
		browserRevokerSeq = append(browserRevokerSeq, name)
	}
	browserRevokers[name] = revoker
}

// Makes the credential current and ends every grant made under the one it replaces, counts by revoker name
func BrowserCredentialRotate(credential string) *pyjson.Object {
	setSessionToken(credential)
	browserRevokersMu.Lock()
	names := append([]string(nil), browserRevokerSeq...)
	revokers := make(map[string]func() int, len(browserRevokers))
	for k, v := range browserRevokers {
		revokers[k] = v
	}
	browserRevokersMu.Unlock()
	ended := pyjson.NewObject()
	for _, name := range names {
		ended.Set(name, revokers[name]())
	}
	return ended
}
