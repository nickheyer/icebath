package dashboard

import (
	"context"
	_ "embed"
)

const previewGuestSources = `default-src 'none'; script-src * data: blob: 'unsafe-inline' 'unsafe-eval' 'wasm-unsafe-eval'; style-src * data: 'unsafe-inline'; img-src * data: blob:; font-src * data:; media-src * data: blob:; connect-src * data: blob:; worker-src * data: blob:; frame-src * data: blob: about:; object-src 'none'; base-uri * about:; frame-ancestors 'self'`

const (
	previewPolicy = previewGuestSources + "; sandbox allow-scripts; form-action 'none'"
	guestSandbox  = "allow-downloads allow-forms allow-orientation-lock allow-pointer-lock allow-presentation allow-scripts"
	guestPolicy   = previewGuestSources + "; sandbox " + guestSandbox + "; form-action *"
)

// Preview shell document relaying authorized content between the parent and its guest frame
//
//go:embed webapp_preview_shell.html
var previewHTML string

// Loader script posting a frame's preview HTML into the shell
const previewLoader = `const frame = document.querySelector('iframe[data-preview-html]');
function send() {
  frame.contentWindow.postMessage({type: 'hermes-preview-document',
    html: frame.dataset.previewHtml, scripts: true}, '*');
}
frame.addEventListener('load', send);
send();
`

// Mounts the credential free preview shell, guest shell and loader on the browser files surface
func mountPreviewAssets(application *FastAPI) {
	deps := []*Dependency{RouteOn("browser_files")}
	application.AddAPIRoute("/assets/webapp-preview.html", []string{"GET"}, &Endpoint{
		Name: "preview_document", Deps: deps,
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return HTMLResponse(previewHTML, 200, "Content-Security-Policy", previewPolicy, "Cache-Control", "no-store",
				"Referrer-Policy", "no-referrer", "X-Content-Type-Options", "nosniff"), nil
		},
	})
	application.AddAPIRoute("/assets/webapp-guest.html", []string{"GET"}, &Endpoint{
		Name: "guest_document", Deps: deps,
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return HTMLResponse(previewHTML, 200, "Content-Security-Policy", guestPolicy, "Cache-Control", "no-store",
				"Referrer-Policy", "no-referrer", "X-Content-Type-Options", "nosniff"), nil
		},
	})
	application.AddAPIRoute("/assets/webapp-preview.js", []string{"GET"}, &Endpoint{
		Name: "preview_loader", Deps: deps,
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return NewResponse([]byte(previewLoader), 200, "text/javascript", "Cache-Control", "no-store",
				"X-Content-Type-Options", "nosniff"), nil
		},
	})
}
