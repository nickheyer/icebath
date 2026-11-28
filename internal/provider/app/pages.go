package app

import (
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside Providers that a search of Settings finds: the page of each provider of the profile, and over it the page of each of its
// accounts
func providerPages(kit *tui.Kit, service providerv1connect.ProviderServiceClient, name string) []tui.Page {
	listed, err := service.List(kit.Env.Ctx, &providerv1.ListRequest{Profile: name})
	if err != nil {
		return nil
	}
	var pages []tui.Page
	for _, entry := range listed.GetProviders() {
		key := providerconfig.Key + "." + entry.GetName()
		pages = append(pages, tui.Page{Title: entry.GetDisplayName(), About: entry.GetStanding(), Words: entry.GetName() + " " + entry.GetSpec(),
			Argument: LoginsOf + entry.GetName(), Keys: []string{key + "."}})
		for _, account := range entry.GetAccounts() {
			pages = append(pages, tui.Page{Title: provider.AccountTitle(entry, account.GetName()), Trail: []string{entry.GetDisplayName()},
				About: "An account of " + entry.GetDisplayName(), Words: account.GetName(), Argument: AccountOf + entry.GetName() + ":" + account.GetName(),
				Keys: []string{key + ".accounts." + account.GetName() + "."}})
		}
	}
	return pages
}

// Pages inside Model list that a search of Settings finds: the limits of each model or provider you set
func limitPages(kit *tui.Kit, service providerv1connect.ProviderServiceClient, name string) []tui.Page {
	limits, err := service.Limits(kit.Env.Ctx, &providerv1.LimitsRequest{Profile: name})
	if err != nil {
		return nil
	}
	var pages []tui.Page
	for _, limit := range limits.GetLimits() {
		title := limit.GetModel()
		if title == "" {
			title = "Every model of " + limit.GetProviderTitle()
		}
		pages = append(pages, tui.Page{Title: title, Trail: []string{"Limits and abilities"}, About: "Limits of " + limitName(limit),
			Words: limit.GetProvider() + " " + limit.GetProviderTitle(), Argument: LimitOf + limit.GetProvider() + ":" + limit.GetModel()})
	}
	return pages
}
