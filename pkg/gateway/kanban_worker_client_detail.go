//go:build !icebath_core && !icebath_noextras

package gateway

import (
	"errors"
	"fmt"

	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

// Quoted cause of a give up, a client error's own message else the exception name and a rejected handshake's HTTP status
func kanbanSilentDetail(err error) string {
	var gce *GatewayClientError
	if errors.As(err, &gce) {
		return err.Error()
	}
	detail := errorutil.Kind(err)
	var status *InvalidStatusError
	if errors.As(err, &status) && status.StatusCode != 0 {
		detail += fmt.Sprintf(" (HTTP %d)", status.StatusCode)
	}
	return detail
}
