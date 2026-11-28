package app

import (
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside Sandbox that a search of Settings finds: the page of each place commands run in. Reading the places looks for container
// programs on this computer, which takes time, so they come as last read while they are read again. An option whose picker opens a page of
// its own, such as Key proxy, is found as that option, whose control is the page
func sandboxPages(kit *tui.Kit, service terminalv1connect.BackendServiceClient, name string) []tui.Page {
	return tui.Remembered("sandbox:"+name, func() []tui.Page {
		response, err := service.Backends(kit.Env.Ctx, &terminalv1.BackendsRequest{Profile: name})
		if err != nil {
			return nil
		}
		pages := []tui.Page{}
		for _, place := range response.GetBackends() {
			page := tui.Page{Title: place.GetTitle(), About: place.GetAbout(), Words: place.GetName(), Argument: place.GetName()}
			if place.GetSection() != "" {
				page.Keys = []string{place.GetSection() + "."}
			}
			pages = append(pages, page)
		}
		return pages
	})
}
