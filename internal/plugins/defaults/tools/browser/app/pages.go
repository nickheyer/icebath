package app

import (
	browserv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1/browserv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside Browser that a search of Settings finds: the page of each service that opens pages
func servicePages(kit *tui.Kit, service browserv1connect.BrowserServiceClient, name string) []tui.Page {
	response, err := service.Services(kit.Env.Ctx, &browserv1.ServicesRequest{Profile: name})
	if err != nil {
		return nil
	}
	var pages []tui.Page
	for _, place := range response.GetServices() {
		page := tui.Page{Title: place.GetTitle(), Words: place.GetName(), Argument: place.GetName()}
		if place.GetSection() != "" {
			page.Keys = []string{place.GetSection() + "."}
		}
		pages = append(pages, page)
	}
	return pages
}
