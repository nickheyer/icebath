package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/web/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/tools/document"
	"github.com/nickheyer/icebath/internal/web"
)

// Pages of URLs that passed the safety checks: fresh cached ones as they are, the rest read by the backend, in the order asked
func (r *registrant) pages(ctx context.Context, call *tools.Call, backend web.Backend, urls []string, format string) ([]*web.Page, error) {
	cfg := call.Env.Config
	cached := map[int]*web.Page{}
	var fetch []string
	var positions []int
	for i, url := range urls {
		var hit *web.Page
		if web.Blocked(cfg, url) == nil {
			hit = r.cached(cfg, url, format, backend.Name)
		}
		if hit != nil {
			cached[i] = hit
			continue
		}
		fetch = append(fetch, url)
		positions = append(positions, i)
	}
	if len(fetch) == 0 {
		return merge(len(urls), cached, nil, nil, nil), nil
	}
	read, err := r.dispatch(ctx, cfg, backend, fetch, format)
	if err != nil {
		return nil, err
	}
	if len(cached) == 0 {
		return read, nil
	}
	return merge(len(urls), cached, positions, fetch, read), nil
}

// Pages the backend reads, a document among them read as text, every page under a URL that was not asked for checked again, and the
// pages the backend itself read kept in the cache under the URL asked for
func (r *registrant) dispatch(ctx context.Context, cfg *config.Config, backend web.Backend, urls []string, format string) ([]*web.Page, error) {
	pages, rescued, err := r.read(ctx, cfg, backend, urls, format)
	if err != nil {
		return nil, err
	}
	asked := map[string]bool{}
	for _, url := range urls {
		asked[url] = true
	}
	for i, page := range pages {
		if page.URL != "" && !asked[page.URL] && page.Error == "" {
			pages[i] = recheck(cfg, page)
		}
		r.document(ctx, cfg, pages[i])
	}
	if rescued {
		return pages, nil
	}
	for _, page := range pages {
		url := ""
		for _, candidate := range []string{page.URL, page.Source} {
			if asked[candidate] {
				url = candidate
				break
			}
		}
		if url != "" && page.Content != "" && page.Error == "" && r.store(cfg, url, page.Content, page.Title, format, backend.Name) && page.URL != "" && page.URL != url {
			r.redirected(cfg, url, page.URL, page.Content, format, backend.Name)
		}
	}
	return pages, nil
}

// The page refused when the URL it ended at after redirects is on the website blocklist or private, the page itself otherwise
func recheck(cfg *config.Config, page *web.Page) *web.Page {
	if block := web.Blocked(cfg, page.URL); block != nil {
		return &web.Page{URL: page.URL, Title: page.Title, Error: block.Message, Blocked: block}
	}
	if !web.SafeURL(cfg, page.URL) {
		return &web.Page{URL: page.URL, Title: page.Title, Error: web.BlockedPrivate}
	}
	return page
}

// Pages the backend reads within the page read timeout, read once more on the free ring when the call fails as a whole and rescue
// applies; rescued says the ring read them
func (r *registrant) read(ctx context.Context, cfg *config.Config, backend web.Backend, urls []string, format string) ([]*web.Page, bool, error) {
	limit := time.Duration(cfg.GetInt(webconfig.ExtractTimeout)) * time.Second
	run, cancel := ctx, context.CancelFunc(func() {})
	if limit > 0 {
		run, cancel = context.WithTimeout(ctx, limit)
	}
	defer cancel()
	type outcome struct {
		pages []*web.Page
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		pages, err := r.readOnce(run, cfg, backend, urls, format)
		done <- outcome{pages, err}
	}()
	var out outcome
	select {
	case out = <-done:
	case <-run.Done():
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		failed := web.Unreadable(urls, fmt.Sprintf("Extract timed out after %ds via %s", int(limit.Seconds()), backend.Name))
		if !r.backends.Rescues(cfg, backend) {
			return failed, false, nil
		}
		return r.rescue(ctx, cfg, backend, urls, failed)
	}
	switch {
	case out.err != nil && !r.backends.Rescues(cfg, backend):
		return nil, false, out.err
	case out.err != nil:
		return r.rescue(ctx, cfg, backend, urls, web.Unreadable(urls, out.err.Error()))
	case wholeBatchFailed(out.pages) && r.backends.Rescues(cfg, backend):
		return r.rescue(ctx, cfg, backend, urls, out.pages)
	}
	return out.pages, false, nil
}

func (r *registrant) rescue(ctx context.Context, cfg *config.Config, backend web.Backend, urls []string, failed []*web.Page) ([]*web.Page, bool, error) {
	pages, err := r.backends.RescueExtract(ctx, cfg, backend.Name, urls, failed)
	return pages, err == nil, err
}

// One page read of the backend: on its free plan through the ring when its calls go there, else through its own API
func (r *registrant) readOnce(ctx context.Context, cfg *config.Config, backend web.Backend, urls []string, format string) ([]*web.Page, error) {
	if ctx.Err() != nil {
		return web.Unreadable(urls, "Interrupted"), nil
	}
	if backend.Free != nil && backend.Free.Used != nil && backend.Free.Used(cfg) {
		pages, err := r.backends.FreeExtract(ctx, cfg, backend.Name, urls)
		if err != nil && !backend.Free.Raises {
			return web.Unreadable(urls, web.Failure(backend.Title, "extract", err)), nil
		}
		return pages, err
	}
	return backend.Extract(ctx, cfg, slices.Clone(urls), format)
}

// Whether every page failed and none of the failures is the page's own HTTP answer
func wholeBatchFailed(pages []*web.Page) bool {
	return len(pages) > 0 && !slices.ContainsFunc(pages, func(page *web.Page) bool { return page.Error == "" || web.Origin(page.Error) })
}

// Reads a page that came back as a document file, a PDF or an office document, as text: its bytes downloaded through the same
// safety checks and converted. A page that cannot be read so is left as it came, or carries why the document could not be read
func (r *registrant) document(ctx context.Context, cfg *config.Config, page *web.Page) {
	if page.Error != "" || page.Content == "" {
		return
	}
	kind := documentKind(page.Content)
	if kind == "" {
		return
	}
	data, err := download(ctx, cfg, page.URL)
	if err != nil {
		if binaryKind(page.Content) == "" {
			page.Content = ""
			page.Error = fmt.Sprintf("URL returned a %s, which could not be downloaded to read as text: %v. Download it with the terminal (curl -L -o) and read the file.", kind, err)
		}
		return
	}
	text, err := r.documents.Extract(ctx, data, documentName(page.URL, kind), r.backends.Parse(cfg))
	switch {
	case err == nil:
		page.Content = text
	case binaryKind(page.Content) == "":
		page.Content = ""
		page.Error = fmt.Sprintf("URL returned a %s that could not be read as text: %v", kind, err)
	}
}

// Bytes at the URL, fetched through the website blocklist and the private address checks at every redirect
func download(ctx context.Context, cfg *config.Config, raw string) ([]byte, error) {
	if block := web.Blocked(cfg, raw); block != nil {
		return nil, errors.New(block.Message)
	}
	if !web.SafeURL(cfg, raw) {
		return nil, errors.New(web.BlockedPrivate)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", provider.UserAgent("icebath"))
	response, err := web.Client(cfg, 60*time.Second, true).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, document.MaxBytes+1))
}

// File name of the document at the URL, given the extension of its kind when its own does not name one the reader knows
func documentName(raw, kind string) string {
	name := "document"
	if u, err := url.Parse(raw); err == nil && path.Base(u.Path) != "/" && path.Base(u.Path) != "." {
		name = path.Base(u.Path)
	}
	if document.Is(nil, name) {
		return name
	}
	return name + extensions[kind]
}
