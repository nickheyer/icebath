package app

import (
	vaultconfig "github.com/nickheyer/icebath/internal/vault/config"
	vaultv1 "github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1/vaultv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside Autofill vault that a search of Settings finds: the vault key, each item of this vault by its name and site, never what it
// holds, and the page of each password manager with its settings. The items are read from the vault, which may wait on the keychain, so
// the pages answer from their last reading and are read again behind
func vaultPages(kit *tui.Kit, service vaultv1connect.VaultServiceClient, name string) []tui.Page {
	key := tui.Page{Title: "Vault key", About: "Where the vault key is kept", Words: "keychain file encryption", Argument: KeyOf,
		Keys: []string{vaultconfig.KeyStorage}}
	return append([]tui.Page{key}, tui.Remembered("vault:"+name, func() []tui.Page { return readPages(kit, service, name) })...)
}

// Pages of the items and the password managers as the vault lists them now, nil when it can't be read, which keeps those read before
func readPages(kit *tui.Kit, service vaultv1connect.VaultServiceClient, name string) []tui.Page {
	listed, err := service.List(kit.Env.Ctx, &vaultv1.ListRequest{Profile: name, Bare: true})
	if err != nil {
		return nil
	}
	pages := []tui.Page{}
	for _, item := range listed.GetItems() {
		pages = append(pages, tui.Page{Title: item.GetLabel(), About: withArticle(kinds[item.GetKind()]), Words: item.GetOrigin() + " " + item.GetIdentifier(),
			Argument: ItemOf + item.GetHandle()})
	}
	for _, source := range listed.GetSources() {
		pages = append(pages, tui.Page{Title: source.GetDisplayName(), About: "A password manager",
			Words: source.GetName() + " password manager logins", Argument: source.GetName(), Keys: []string{managerSection(source.GetName()) + "."}})
	}
	return pages
}
