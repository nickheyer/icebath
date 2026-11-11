package gateway

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Whether a value has the sixteen hex character shape of a pending request id
func PairingLooksLikeRequestID(value string) bool {
	value = pystr.Strip(value)
	if len(value) != 16 {
		return false
	}
	for _, c := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// Whether code approvals on the platform are refused after too many failed attempts
func (s *PairingStore) IsLockedOut(platform string) (bool, error) {
	return s.isLockedOut(platform)
}
