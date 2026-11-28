package app

import (
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	webv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/web/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/web/v1/webv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Argument of the web search screen that opens the page of the blocked sites
const BlockedOf = "blocked"

// Pages inside Web search that a search of Settings finds: the page of each service, and Blocked sites
func servicePages(kit *tui.Kit, service webv1connect.WebServiceClient, name string) []tui.Page {
	var pages []tui.Page
	if response, err := service.List(kit.Env.Ctx, &webv1.ListRequest{Profile: name}); err == nil {
		for _, backend := range response.GetBackends() {
			page := tui.Page{Title: backend.GetTitle(), Words: backend.GetName(), Argument: backend.GetName()}
			if backend.GetSection() != "" {
				page.Keys = []string{backend.GetSection() + "."}
			}
			pages = append(pages, page)
		}
	}
	return append(pages, tui.Page{Title: "Blocked sites", About: "Sites that web tools refuse to open", Words: "blocklist", Argument: BlockedOf,
		Keys: []string{webconfig.Blocklist + "."}})
}
