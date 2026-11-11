package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/hostinfo"
)

var machineRegistry = &handlerRegistry{}

func init() {
	machineRegistry.method("machine.facts", machineFacts)
	machineRegistry.install()
}

// Host summary the desktop first run onboarding branches on, without the account's full name
func machineFacts(ctx context.Context, rid any, params map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range hostinfo.HostSummary().AsMap() {
		if key != "full_name" {
			result[key] = value
		}
	}
	return ok(rid, result)
}
