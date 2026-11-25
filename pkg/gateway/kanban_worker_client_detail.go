//go:build !icebath_core && !icebath_noextras

package gateway

import (
	"github.com/nickheyer/icebath/pkg/errchain"
)

// Cause of a give up in plain words: the error's own message, an operating system failure as its reason and path
func kanbanSilentDetail(err error) string {
	return errchain.OSText(err)
}
