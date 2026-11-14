package dashboard

import "testing"

// Fresh application of web_server.py under a temp home, the TestClient(web_server.app) of the hermes cases
func dashboardTestApp(t *testing.T) *FastAPI {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetMultiProfileLatch(t)
	installProviderRegistries(t)
	if err := ImportWebServer(); err != nil {
		t.Fatal(err)
	}
	return buildWebServerApp(includeWebRouters)
}
