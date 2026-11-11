package dashboard

import "testing"

// Application of web_server.py with the routers in the tree, the TestClient(web_server.app) of the hermes cases
func dashboardTestApp(t *testing.T) *FastAPI {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetMultiProfileLatch(t)
	installProviderRegistries(t)
	if err := ImportWebServer(); err != nil {
		t.Fatal(err)
	}
	return buildWebServerApp(testIncludeWebRouters)
}
