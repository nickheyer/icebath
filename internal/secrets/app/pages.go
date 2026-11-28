package app

import (
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside Secret stores that a search of Settings finds: the order of the stores, the page of each store, the parts of a store, and
// the variables of a store that fills named ones. A part comes before its store, so a setting of the part opens the part
func storePages(kit *tui.Kit, service secretsv1connect.SecretServiceClient, stores []Store, name string) []tui.Page {
	response, err := service.Sources(kit.Env.Ctx, &secretsv1.SourcesRequest{Profile: name, Bare: true})
	if err != nil {
		return nil
	}
	pages := []tui.Page{{Title: "Store order", About: "The order the stores are asked in", Words: "order first", Argument: OrderOf,
		Keys: []string{secretsconfig.Sources}}}
	for _, source := range response.GetSources() {
		label, store := source.GetLabel(), source.GetName()
		for _, own := range stores {
			if own.Source != store {
				continue
			}
			for _, part := range own.Parts {
				pages = append(pages, tui.Page{Title: part.Title, Trail: []string{label}, About: "A part of " + label, Argument: store + ":" + part.Section,
					Keys: []string{part.Section + "."}})
			}
		}
		pages = append(pages, tui.Page{Title: label, About: "A secret store", Words: store, Argument: store, Keys: []string{source.GetSection() + "."}})
		if source.GetShape() == "mapped" {
			pages = append(pages, tui.Page{Title: "Variables", Trail: []string{label}, About: "The variables " + label + " fills", Words: "references",
				Argument: store + VariablesOf, Keys: []string{secretsconfig.Of(store, secretsconfig.Env)}})
		}
	}
	return pages
}
