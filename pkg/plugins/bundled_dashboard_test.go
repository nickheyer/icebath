package plugins

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestBundledDashboardRegistryKeepsOrderAndRefusesDuplicates(t *testing.T) {
	bundledDashboardMu.Lock()
	prev := bundledDashboardOrder
	bundledDashboardOrder = nil
	bundledDashboardMu.Unlock()
	t.Cleanup(func() {
		bundledDashboardMu.Lock()
		bundledDashboardOrder = prev
		bundledDashboardMu.Unlock()
	})
	api := func(ctx context.Context) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok " + r.URL.Path)) })
	}
	assets := fstest.MapFS{"dist/index.js": {Data: []byte("x")}}
	RegisterBundledDashboard(BundledDashboard{Name: "beta", Manifest: []byte(`{"name":"beta"}`), Assets: assets, API: api})
	RegisterBundledDashboard(BundledDashboard{Name: "alpha", Manifest: []byte(`{"name":"alpha"}`)})
	got := BundledDashboards()
	if len(got) != 2 || got[0].Name != "beta" || got[1].Name != "alpha" || string(got[0].Manifest) != `{"name":"beta"}` {
		t.Fatalf("registry order or content: %+v", got)
	}
	if data, err := fstestRead(got[0]); err != nil || data != "x" {
		t.Fatalf("assets = %q %v", data, err)
	}
	rec := httptest.NewRecorder()
	got[0].API(context.Background()).ServeHTTP(rec, httptest.NewRequest("GET", "/stats", nil))
	if rec.Body.String() != "ok /stats" {
		t.Fatalf("api body %q", rec.Body.String())
	}
	got[0].Name = "mutated"
	if BundledDashboards()[0].Name != "beta" {
		t.Fatal("BundledDashboards exposed its backing slice")
	}
	for _, d := range []BundledDashboard{{Name: "beta"}, {}} {
		func() {
			defer func() {
				r := recover()
				if r == nil || !strings.Contains(r.(string), "plugins: ") {
					t.Fatalf("registering %q recovered %v", d.Name, r)
				}
			}()
			RegisterBundledDashboard(d)
		}()
	}
	if len(BundledDashboards()) != 2 {
		t.Fatal("a refused registration was recorded")
	}
}

func fstestRead(d BundledDashboard) (string, error) {
	f, err := d.Assets.Open("dist/index.js")
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 8)
	n, _ := f.Read(buf)
	return string(buf[:n]), nil
}
