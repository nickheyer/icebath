package dashboard

// Routers of web_server.py L816 to L853 that are in the tree, in the Python include order with their dependencies
func testIncludeWebRouters(app *FastAPI) {
	app.IncludeRouter(filesRouter(), "")
	app.IncludeRouter(uploadsRouter(), "", RouteOn("browser_files"))
	app.IncludeRouter(desktopPluginsRouter(), "", RouteOn("desktop_plugins"))
	app.IncludeRouter(gitRouter(), "")
	app.IncludeRouter(displayRouter(), "")
	app.IncludeRouter(sessionsListRouter(), "")
	app.IncludeRouter(sessionsSearchRouter(), "")
	app.IncludeRouter(configEnvConfigRouter(), "")
	app.IncludeRouter(configEnvRouter(), "")
	app.IncludeRouter(sessionsManageRouter(), "")
	app.IncludeRouter(cronRouter(), "")
	app.IncludeRouter(mcpRouter(), "")
	app.IncludeRouter(chatWorkspacesRouter(), "")
	app.IncludeRouter(sharedMetricsRouter(), "")
	app.IncludeRouter(delegationRouter(), "")
	app.IncludeRouter(webappAccessRouter(), "", RouteOn("web_access"))
	app.IncludeRouter(uiPreferencesRouter(), "", RouteOn("ui_preferences"))
}
