package app

import (
	"strings"

	generatev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1/generatev1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside Images or Videos that a search of Settings finds: the page of each service, which shows the settings of its section and its
// API key, read bare so no account is asked how it stands
func servicePages(kit *tui.Kit, service generatev1connect.GenerateServiceClient, k kind, name string) []tui.Page {
	response, err := service.List(kit.Env.Ctx, &generatev1.ListRequest{Profile: name, Kind: k.proto, Bare: true})
	if err != nil {
		return nil
	}
	var pages []tui.Page
	for _, s := range response.GetServices() {
		page := tui.Page{Title: s.GetTitle(), About: "A service that makes " + k.noun, Words: s.GetName() + " " + s.GetDescription(), Argument: s.GetName(),
			Keys: []string{s.GetSection() + "."}}
		// A key kept under another area, as FAL's under Images, is found on the page of that area
		if key := s.GetKey(); strings.HasPrefix(key, k.area+".") {
			page.Keys = append(page.Keys, key)
		}
		pages = append(pages, page)
	}
	return pages
}
