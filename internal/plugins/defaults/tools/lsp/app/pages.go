package app

import (
	"strings"

	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1/lspv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside Language servers that a search of Settings finds: the page of each server
func serverPages(kit *tui.Kit, service lspv1connect.LspServiceClient, name string) []tui.Page {
	response, err := service.Status(kit.Env.Ctx, &lspv1.StatusRequest{Profile: name})
	if err != nil {
		return nil
	}
	var pages []tui.Page
	for _, server := range response.GetServers() {
		pages = append(pages, tui.Page{Title: server.GetId(), About: server.GetDescription(), Words: strings.Join(server.GetExtensions(), " "),
			Argument: server.GetId(), Keys: []string{lspconfig.Servers + "." + server.GetId() + "."}})
	}
	return pages
}
