//go:build windows

package gateway

import "time"

// One runtime control exchange with the owner of home through its native control channel
func ownerControlExchange(home string, request []byte, deadline time.Time) ([]byte, error) {
	return queryRuntimeControl(home, request, time.Until(deadline))
}
