package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const immutableAssetCacheControl = "public, max-age=31536000, immutable"

var noStoreHeaders = []string{"Cache-Control", "no-store, no-cache, must-revalidate"}

const headlessMsg = "Headless backend (icebath serve): web UI disabled — use " +
	"`icebath dashboard` for the browser UI."

// JSON text of a value as json.dumps writes it
func jsonDumps(v any) string {
	s, err := pyjson.Dumps(v)
	if err != nil {
		panic(err)
	}
	return s
}

// True when p resolved stays under root resolved
func resolvedUnder(p, root string) bool {
	rp, err := pypath.Realpath(p, pypath.Lax)
	if err != nil {
		return false
	}
	rr, err := pypath.Realpath(root, pypath.Lax)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rr, rp)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// True for an existing regular file after following links
func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Mounts the built SPA with its asset routes, unmatched paths falling back to index.html
func mountSPA(application *FastAPI) {
	if os.Getenv("ICEBATH_SERVE_HEADLESS") == "1" {
		application.AddAPIRoute("/{full_path:path}", []string{"GET"}, &Endpoint{
			Name:   "no_frontend",
			Params: []Param{PathParam("full_path", Str)},
			Handle: func(ctx context.Context, c *Call) (any, error) {
				gated := application.State.Bool("auth_required")
				withheld := gated || application.State.Bool("withhold_session_token")
				if c.Str("full_path") == "" && !withheld {
					return HTMLResponse("<!doctype html><html><head><script>"+
						"window.__HERMES_SESSION_TOKEN__="+jsonDumps(SessionToken())+";"+
						"window.__HERMES_AUTH_REQUIRED__=false;"+
						"</script></head><body>"+headlessMsg+"</body></html>", 200, noStoreHeaders...), nil
				}
				return JSONResponse(pyjson.ObjectOf("error", headlessMsg), 404)
			},
		})
		return
	}
	webDist := func() string {
		if d, ok := application.State.Get("web_dist").(string); ok && d != "" {
			return d
		}
		return WebDist()
	}
	surfaceOf := func() string {
		if s, ok := application.State.Get("ui_surface").(string); ok {
			return s
		}
		return "dashboard"
	}
	serveIndex := func(ctx context.Context, req *Request, prefix string) (any, error) {
		raw, err := os.ReadFile(filepath.Join(webDist(), "index.html"))
		if err != nil {
			missing := "Frontend not built. Run: cd web && npm run build"
			if surfaceOf() == "webapp" {
				missing = "The Webapp is not built. Run: icebath webapp"
			}
			return JSONResponse(pyjson.ObjectOf("error", missing), 404)
		}
		html, err := pycodecs.Decode(raw, "utf-8-sig")
		if err != nil {
			return nil, err
		}
		if surfaceOf() == "webapp" {
			if stale := gateway.BundleStaleReason(webDist()); stale != "" {
				return JSONResponse(pyjson.ObjectOf("error", stale), 503, noStoreHeaders...)
			}
		}
		chatJS := "false"
		if dashboardEmbeddedChatEnabled {
			chatJS = "true"
		}
		gated := application.State.Bool("auth_required")
		surfaceJS := ""
		if surface, ok := application.State.Lookup("ui_surface"); ok && surface != nil {
			surfaceJS = "window.__HERMES_UI_SURFACE__=" + jsonDumps(surface) + ";"
		}
		withheld := gated || application.State.Bool("withhold_session_token") || !PolicyOf(application.State).HTMLToken
		tokenJS := ""
		if !withheld {
			tokenJS = `window.__HERMES_SESSION_TOKEN__="` + SessionToken() + `";`
		}
		initial := ""
		if v := application.State.Get("initial_profile"); pyTruthy(v) {
			initial = pyStrOf(v)
		}
		initialProfileJS := strings.ReplaceAll(jsonDumps(initial), "</", `<\/`)
		servingProfileJS := strings.ReplaceAll(jsonDumps(servingProfileName(ctx)), "</", `<\/`)
		authJS := "false"
		if gated {
			authJS = "true"
		}
		bootstrap := "<script>" + tokenJS +
			"window.__HERMES_DASHBOARD_EMBEDDED_CHAT__=" + chatJS + ";" +
			`window.__HERMES_BASE_PATH__="` + prefix + `";` +
			"window.__HERMES_AUTH_REQUIRED__=" + authJS + ";" +
			"window.__HERMES_INITIAL_PROFILE__=" + initialProfileJS + ";" +
			surfaceJS +
			"window.__HERMES_DASHBOARD_PROFILE__=" + servingProfileJS + ";" +
			"</script>"
		if prefix != "" {
			for _, attr := range []string{`href="/assets/`, `src="/assets/`, `href="/favicon.ico"`, `href="/fonts/`,
				`href="/ds-assets/`, `src="/ds-assets/`} {
				html = strings.ReplaceAll(html, attr, strings.Replace(attr, `"/`, `"`+prefix+`/`, 1))
			}
		}
		if theme := renderActiveThemeBootstrapCSS(ctx); theme != "" {
			html = strings.Replace(html, "</head>", theme+"</head>", 1)
		}
		html = strings.Replace(html, "</head>", bootstrap+"</head>", 1)
		return HTMLResponse(html, 200, pageHeaders(req, html, noStoreHeaders)...), nil
	}
	application.AddAPIRoute("/assets/{filename}.css", []string{"GET"}, &Endpoint{
		Name:   "serve_css",
		Params: []Param{PathParam("filename", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			dist := webDist()
			cssPath := filepath.Join(dist, "assets", c.Str("filename")+".css")
			if !isRegularFile(cssPath) || !resolvedUnder(cssPath, dist) {
				return JSONResponse(pyjson.ObjectOf("error", "not found"), 404)
			}
			prefix := gateway.NormalisePrefix(c.Request.Headers().Get("x-forwarded-prefix"))
			raw, err := os.ReadFile(cssPath)
			if err != nil {
				return nil, err
			}
			css, err := pycodecs.Decode(raw, "utf-8-sig")
			if err != nil {
				return nil, err
			}
			if prefix != "" {
				for _, assetDir := range []string{"/fonts/", "/fonts-terminal/", "/ds-assets/", "/assets/"} {
					for _, quote := range []string{"", `"`, "'"} {
						css = strings.ReplaceAll(css, "url("+quote+assetDir, "url("+quote+prefix+assetDir)
					}
				}
			}
			return NewResponse([]byte(css), 200, "text/css", "Cache-Control", immutableAssetCacheControl), nil
		},
	})
	var assetFiles sync.Map
	serveAssets := func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		directory := filepath.Join(webDist(), "assets")
		files, _ := assetFiles.LoadOrStore(directory, &StaticFiles{Directory: directory,
			Override: func(ctx context.Context, s *StaticFiles, path string, scope *Scope) (ASGIResponse, error) {
				resp, err := s.GetResponse(ctx, path, scope)
				if err == nil && resp.Status() == 200 {
					resp.Header().Set("Cache-Control", immutableAssetCacheControl)
				}
				return resp, err
			}})
		return files.(*StaticFiles).ASGI(ctx, scope, receive, send)
	}
	mountPreviewAssets(application)
	application.Mount("/assets", serveAssets, "assets")
	application.AddAPIRoute("/{full_path:path}", []string{"GET"}, &Endpoint{
		Name:   "serve_spa",
		Params: []Param{PathParam("full_path", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			fullPath := c.Str("full_path")
			prefix := gateway.NormalisePrefix(c.Request.Headers().Get("x-forwarded-prefix"))
			if fullPath == "api" || strings.HasPrefix(fullPath, "api/") {
				return JSONResponse(pyjson.ObjectOf("detail", "No such API endpoint: /"+fullPath), 404)
			}
			dist := webDist()
			filePath := filepath.Join(dist, fullPath)
			index := filepath.Join(dist, "index.html")
			if !PolicyOf(application.State).HTMLToken && isRegularFile(filePath) && isRegularFile(index) {
				a, errA := os.Stat(filePath)
				b, errB := os.Stat(index)
				if errA == nil && errB == nil && os.SameFile(a, b) {
					return serveIndex(ctx, c.Request, prefix)
				}
			}
			if fullPath != "" && resolvedUnder(filePath, dist) && isRegularFile(filePath) {
				return NewFileResponse(filePath, 200, "", nil, "attachment"), nil
			}
			if IsStaticAssetPath(fullPath) {
				if font := PublicFontFile(dist, WebDist(), fullPath); font != "" {
					return NewFileResponse(font, 200, "", nil, "attachment"), nil
				}
				return JSONResponse(pyjson.ObjectOf("error", "not found"), 404)
			}
			return serveIndex(ctx, c.Request, prefix)
		},
	})
}
