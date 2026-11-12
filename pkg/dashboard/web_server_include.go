package dashboard

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/gateway"
)

var (
	webAppMu     sync.Mutex
	webApp       *FastAPI
	webAppLoaded atomic.Bool
)

// Mounts the routers in route matching order, literal paths ahead of their templated siblings
func includeWebRouters(app *FastAPI) {
	app.IncludeRouter(filesRouter(), "")
	app.IncludeRouter(uploadsRouter(), "", RouteOn("browser_files"))
	app.IncludeRouter(desktopPluginsRouter(), "", RouteOn("desktop_plugins"))
	app.IncludeRouter(gitRouter(), "")
	app.IncludeRouter(localModelsRouter(), "")
	app.IncludeRouter(statusRouter(), "")
	app.IncludeRouter(actionsRouter(), "")
	app.IncludeRouter(audioRouter(), "")
	app.IncludeRouter(displayRouter(), "")
	app.IncludeRouter(actionsStatusRouter(), "")
	app.IncludeRouter(sessionsListRouter(), "")
	app.IncludeRouter(profilesSessionsRouter(), "")
	app.IncludeRouter(sessionsSearchRouter(), "")
	app.IncludeRouter(memoryProvidersRouter(), "")
	app.IncludeRouter(configEnvConfigRouter(), "")
	app.IncludeRouter(modelsRouter(), "")
	app.IncludeRouter(configEnvRouter(), "")
	app.IncludeRouter(messagingRouter(), "")
	app.IncludeRouter(oauthRouter(), "")
	app.IncludeRouter(sessionsManageRouter(), "")
	app.IncludeRouter(statusLogsRouter(), "")
	app.IncludeRouter(cronRouter(), "")
	app.IncludeRouter(mcpRouter(), "")
	app.IncludeRouter(opsRouter(), "")
	app.IncludeRouter(skillsHubRouter(), "")
	app.IncludeRouter(profilesRouter(), "")
	app.IncludeRouter(skillsRouter(), "")
	app.IncludeRouter(toolsRouter(), "")
	app.IncludeRouter(analyticsRouter(), "")
	app.IncludeRouter(chatWsRouter(), "")
	app.IncludeRouter(chatWorkspacesRouter(), "")
	app.IncludeRouter(dashboardUiRouter(), "")
	app.IncludeRouter(webappRouter(), "")
	app.IncludeRouter(sharedMetricsRouter(), "")
	app.IncludeRouter(delegationRouter(), "")
	app.IncludeRouter(webappAccessRouter(), "", RouteOn("web_access"))
	app.IncludeRouter(uiPreferencesRouter(), "", RouteOn("ui_preferences"))
}

// Application of the web server module with every router mounted, built on first use and again after a failed build
func WebServerApp() (app *FastAPI, err error) {
	webAppMu.Lock()
	defer webAppMu.Unlock()
	if webApp != nil {
		return webApp, nil
	}
	defer func() {
		if r := recover(); r != nil {
			if cause, isErr := r.(error); isErr {
				err = cause
			} else {
				err = fmt.Errorf("%v", r)
			}
			app = nil
		}
	}()
	if err := ImportWebServer(); err != nil {
		return nil, err
	}
	webApp = buildWebServerApp(includeWebRouters)
	webAppLoaded.Store(true)
	return webApp, nil
}

// Whether the module application exists in this process, the web server module being imported
func webServerAppLoaded() bool { return webAppLoaded.Load() }

// Starts the web UI server of the module application and serves until it stops
func StartServer(ctx context.Context, o gateway.DashboardServerOptions) error {
	app, err := WebServerApp()
	if err != nil {
		return err
	}
	return startServer(ctx, app, o)
}
