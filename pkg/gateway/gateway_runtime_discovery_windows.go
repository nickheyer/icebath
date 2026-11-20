//go:build windows

package gateway

import (
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// identify result of home's control pipe, keeping timeout, access and protocol failures as errors
func QueryIdentify(home string, timeout time.Duration) (*pyjson.Object, error) {
	data, err := queryRuntimeControl(home, identifyRequest, timeout)
	if err != nil {
		return nil, err
	}
	return identifyResponse(data)
}

// The lock privacy check runs only where control nodes are POSIX files
func privateLock(lock string) error { return nil }

// Windows homes have no private overlay locks
func overlayOwnerHome(home string) string { return home }
