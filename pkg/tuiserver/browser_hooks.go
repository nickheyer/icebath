package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/linktier"
)

// Browser parts of the session host, bound by the extras tier that carries the browser and nil in a build without it
var (
	browserManageHandler func(ctx context.Context, rid any, params map[string]any) map[string]any
	releaseVaultSession  func(sessionID string)
)

var browserRegistry = &handlerRegistry{}

// Registers browser.manage, refused with the build line when the browser is absent
func init() {
	browserRegistry.scopedMethod("browser.manage", func(ctx context.Context, rid any, params map[string]any) map[string]any {
		if browserManageHandler == nil {
			return rpcErr(rid, 4015, linktier.Missing("browser automation", linktier.Extras), nil)
		}
		return browserManageHandler(ctx, rid, params)
	})
	browserRegistry.install()
}
