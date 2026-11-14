package yuanbao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// MediaResolveMiddleware downloads inbound media into the local cache and hands the model the cached paths
type MediaResolveMiddleware struct{ middlewareBase }

func newMediaResolveMiddleware() InboundMiddleware {
	return &MediaResolveMiddleware{middlewareBase{"MediaResolveMiddleware", "media-resolve"}}
}

const (
	resourceCacheTTLS    = 24 * 60 * 60
	resourceCacheMaxSize = 256
	resourceHTTPTimeout  = 15 * time.Second
)

// One downloaded resource with its MIME type and the wall clock time it was cached
type cachedResource struct {
	path, mime string
	ts         float64
}

// Download cache of the whole process keyed by resource id
var resourceCache = struct {
	sync.Mutex
	entries *pydict.Dict[string, cachedResource]
}{entries: pydict.New[string, cachedResource]()}

// Cached path and MIME of a resource, the entry dropped once it expired or its file is gone
func getCachedResource(resourceID string) (string, string, bool) {
	if resourceID == "" {
		return "", "", false
	}
	resourceCache.Lock()
	defer resourceCache.Unlock()
	entry, ok := resourceCache.entries.Get(resourceID)
	if !ok {
		return "", "", false
	}
	if wallClock()-entry.ts > resourceCacheTTLS || !isFile(entry.path) {
		resourceCache.entries.Delete(resourceID)
		return "", "", false
	}
	return entry.path, entry.mime, true
}

// Caches a download result, evicting the oldest quarter of the entries at capacity
func putCachedResource(resourceID, localPath, mime string) {
	if resourceID == "" {
		return
	}
	resourceCache.Lock()
	defer resourceCache.Unlock()
	entries := resourceCache.entries
	if entries.Len() >= resourceCacheMaxSize {
		keys := entries.Keys()
		sort.SliceStable(keys, func(i, j int) bool {
			left, _ := entries.Get(keys[i])
			right, _ := entries.Get(keys[j])
			return left.ts < right.ts
		})
		for _, key := range keys[:resourceCacheMaxSize/4] {
			entries.Delete(key)
		}
	}
	entries.Set(resourceID, cachedResource{localPath, mime, wallClock()})
}

// Appends a cached resource to the output lists, false on a miss
func appendCachedResource(adapter *YuanbaoAdapter, resourceID string, mediaPaths, mimes *[]string) bool {
	path, mime, ok := getCachedResource(resourceID)
	if !ok {
		return false
	}
	slog.Debug(fmt.Sprintf("[%s] resource cache hit: rid=%s path=%s", adapter.Name(), resourceID, path))
	*mediaPaths = append(*mediaPaths, path)
	*mimes = append(*mimes, mime)
	return true
}

var imageURLExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true,
	".heic": true, ".tiff": true}

// Lowercased image extension of the URL path, jpg for any other extension
func guessImageExtFromURL(rawURL string) (string, error) {
	parsed, err := pyurl.Parse(rawURL)
	if err != nil {
		return "", err
	}
	_, ext := pypath.Splitext(parsed.Path)
	if ext = pystr.Lower(ext); imageURLExts[ext] {
		return ext, nil
	}
	return ".jpg", nil
}

// Auth headers of a resource API request, nil without a token or a bot id
func resourceAuthHeaders(adapter *YuanbaoAdapter, tokenData map[string]any, fallbackSource string) map[string]string {
	token := stripOrEmpty(tokenData["token"])
	botID := pystr.Strip(pyrepr.Str(pyrepr.Or(pyrepr.Or(tokenData["bot_id"], adapter.botIDValue()), adapter.appKey)))
	if token == "" || botID == "" {
		return nil
	}
	source := pystr.Strip(pyrepr.Str(pyrepr.Or(tokenData["source"], fallbackSource)))
	if source == "" {
		source = "web"
	}
	return map[string]string{"Content-Type": "application/json", "X-ID": botID, "X-Token": token, "X-Source": source}
}

// HTTP client of a resource API request, a package var the way hermes tests patch httpx
var resourceHTTPClient = func() *http.Client { return pyhttpx.PlainClient(true) }

// Exchanges a resource id for a direct download URL, retrying once with a fresh token after a 401
func fetchResourceURL(ctx context.Context, adapter *YuanbaoAdapter, resourceID string) (string, error) {
	resourceID = pystr.Strip(resourceID)
	if resourceID == "" {
		return "", runtimeError("missing resource_id")
	}
	tokenData, err := adapter.getCachedToken(ctx)
	if err != nil {
		return "", err
	}
	headers := resourceAuthHeaders(adapter, tokenData, "web")
	if headers == nil {
		return "", runtimeError("missing token or bot_id for resource download")
	}
	apiURL := pyhttpx.MergeParams(adapter.apiDomain+"/api/resource/v1/download", pyhttpx.Params{{"resourceId", resourceID}})
	client := resourceHTTPClient()
	defer client.CloseIdleConnections()
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return "", err
		}
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp, err := pyhttpx.Do(client, req, resourceHTTPTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", requestError(err)
		}
		if resp.StatusCode == 401 && attempt == 0 {
			refreshed, err := signForceRefresh(ctx, adapter.appKey, adapter.appSecret, adapter.apiDomain, "")
			if err != nil {
				return "", err
			}
			if headers = resourceAuthHeaders(adapter, refreshed, headers["X-Source"]); headers == nil {
				break
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", fmt.Errorf("%s", pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.URL, resp.Header.Get("Location")))
		}
		decoded, err := resp.JSON()
		if err != nil {
			return "", err
		}
		payload, err := dictOf(decoded)
		if err != nil {
			return "", err
		}
		code := payload["code"]
		if err := unhashable(code); err != nil {
			return "", err
		}
		if code != nil && !equalsInt(code, 0) {
			return "", runtimeError(fmt.Sprintf("resource/v1/download failed: code=%s, msg=%s", pyrepr.Str(code),
				pyrepr.Str(dictGetOr(payload, "msg", ""))))
		}
		data := payload
		if inner, isDict := asDict(payload["data"]); isDict {
			data = inner
		}
		if realURL := stripOrEmpty(pyrepr.Or(data["url"], data["realUrl"])); realURL != "" {
			return realURL, nil
		}
		return "", runtimeError("resource/v1/download missing url/realUrl")
	}
	return "", runtimeError("resource/v1/download did not return a URL")
}

// Fetchable URL of a resource placeholder URL through the business API, any other URL and a failed lookup unchanged
func resolveDownloadURL(ctx context.Context, adapter *YuanbaoAdapter, rawURL string) (string, error) {
	resourceID := parseResourceID(rawURL)
	if resourceID == "" {
		return rawURL, nil
	}
	resolved, err := fetchResourceURLFn(ctx, adapter, resourceID)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return rawURL, nil
	}
	return resolved, nil
}

// Downloads one resource into the local media cache, ok false when the download or the cache write was refused
func downloadAndCache(ctx context.Context, adapter *YuanbaoAdapter, fetchURL, kind, fileName, logTag, resourceID string) (localPath, mime string, ok bool, err error) {
	if resourceID != "" {
		if path, cachedMime, hit := getCachedResource(resourceID); hit {
			slog.Debug(fmt.Sprintf("[%s] resource cache hit: rid=%s path=%s", adapter.Name(), resourceID, path))
			return path, cachedMime, true, nil
		}
	}
	name := adapter.Name()
	fileBytes, contentType, err := DownloadURL(ctx, gateway.URLSafetyEnv(ctx), fetchURL, mediaMaxSizeMB)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", false, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] inbound media download failed: kind=%s %s err=%s", name, kind, logTag, errText(err)))
		return "", "", false, nil
	}
	switch kind {
	case "image":
		ext, err := guessImageExtFromURL(fetchURL)
		if err != nil {
			return "", "", false, err
		}
		if localPath, err = gateway.CacheImageFromBytesAsync(ctx, fileBytes, ext); err != nil {
			if ctx.Err() != nil || !isValueError(err) {
				return "", "", false, err
			}
			slog.Warn(fmt.Sprintf("[%s] inbound image cache rejected: %s err=%s", name, logTag, errText(err)))
			return "", "", false, nil
		}
		if mime = GuessMIMEType("image" + ext); !strings.HasPrefix(mime, "image/") {
			mime = "image/jpeg"
			if strings.HasPrefix(contentType, "image/") {
				mime = contentType
			}
		}
	case "video":
		if localPath, err = gateway.CacheVideoFromBytesAsync(ctx, fileBytes, ".mp4"); err != nil {
			return "", "", false, err
		}
		mime = GuessMIMEType(localPath)
	default:
		if fileName == "" {
			parsed, err := pyurl.Parse(fetchURL)
			if err != nil {
				return "", "", false, err
			}
			if fileName = pypath.Basename(parsed.Path); fileName == "" {
				fileName = "file"
			}
		}
		if localPath, err = gateway.CacheDocumentFromBytesAsync(ctx, fileBytes, fileName); err != nil {
			if ctx.Err() != nil {
				return "", "", false, ctx.Err()
			}
			slog.Warn(fmt.Sprintf("[%s] inbound file cache failed: %s err=%s", name, logTag, errText(err)))
			return "", "", false, nil
		}
		mime = GuessMIMEType(fileName)
	}
	putCachedResource(resourceID, localPath, mime)
	return localPath, mime, true, nil
}

// Media resolution steps, package vars the way hermes tests patch the middleware methods
var (
	fetchResourceURLFn     = fetchResourceURL
	resolveDownloadURLFn   = resolveDownloadURL
	downloadAndCacheFn     = downloadAndCache
	resolveMediaURLsFn     = resolveMediaURLs
	resolveQuoteMediaFn    = resolveQuoteMedia
	collectObservedMediaFn = collectObservedMedia
)

// One media item to resolve with the key its download URL is looked up by
type resolveItem struct{ kind, filename, rid, key string }

// URL lookup and log wording of one resolve batch
type resolveBatch struct {
	scope   string
	getURL  func(ctx context.Context, key string) (string, error)
	failed  func(kind, key string, err error) string
	crashed func(kind, key string, err error) string
	logTag  func(key string) string
}

// Result of one resolved item, err set only for a failure that escaped the per item handling
type resolveOutcome struct {
	path, mime string
	ok         bool
	err        error
}

// Looks up the URL of one item and downloads it into the cache
func resolveOne(ctx context.Context, adapter *YuanbaoAdapter, item resolveItem, batch resolveBatch) (out resolveOutcome) {
	if panicked := recovered(func() {
		fetchURL, err := batch.getURL(ctx, item.key)
		if err != nil {
			if ctx.Err() != nil {
				out.err = ctx.Err()
				return
			}
			slog.Warn(batch.failed(item.kind, item.key, err))
			return
		}
		out.path, out.mime, out.ok, out.err = downloadAndCacheFn(ctx, adapter, fetchURL, item.kind, item.filename,
			batch.logTag(item.key), item.rid)
	}); panicked != nil {
		out = resolveOutcome{err: panicked}
	}
	return out
}

// Resolves the items under the configured concurrency, appending the successes in input order and logging one batch summary
func gatherResolve(ctx context.Context, adapter *YuanbaoAdapter, active []resolveItem, batch resolveBatch, outPaths, outMimes *[]string) error {
	concurrency := adapter.MediaResolveConcurrency
	results := make([]resolveOutcome, len(active))
	slots := make(chan struct{}, concurrency)
	var running sync.WaitGroup
	started := monotonic()
	for i, item := range active {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			running.Wait()
			return ctx.Err()
		}
		running.Add(1)
		go func() {
			defer running.Done()
			defer func() { <-slots }()
			results[i] = resolveOne(ctx, adapter, item, batch)
		}()
	}
	running.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	elapsedMS := int64((monotonic() - started) * 1000)
	failed := 0
	for i, item := range active {
		result := results[i]
		if result.err != nil {
			slog.Warn(batch.crashed(item.kind, item.key, result.err))
		}
		if result.err != nil || !result.ok {
			failed++
			continue
		}
		*outPaths = append(*outPaths, result.path)
		*outMimes = append(*outMimes, result.mime)
	}
	slog.Info(fmt.Sprintf("[%s] media resolve batch: scope=%s concurrency=%d total=%d ok=%d failed=%d elapsed_ms=%d", adapter.Name(),
		batch.scope, concurrency, len(active), len(*outPaths), failed, elapsedMS))
	return nil
}

// Local paths and MIME types of the resolvable media refs a message carries, cache hits served without a fetch
func resolveMediaURLs(ctx context.Context, adapter *YuanbaoAdapter, mediaRefs []InboundMediaRef) ([]string, []string, error) {
	mediaURLs, mediaTypes := []string{}, []string{}
	var active []resolveItem
	for _, ref := range mediaRefs {
		kind := pystr.Lower(pystr.Strip(ref.Kind))
		rawURL := pystr.Strip(ref.URL)
		if !resolvableMediaKinds[kind] || rawURL == "" {
			continue
		}
		rid := parseResourceID(rawURL)
		if rid != "" && appendCachedResource(adapter, rid, &mediaURLs, &mediaTypes) {
			continue
		}
		active = append(active, resolveItem{kind, pystr.Strip(ref.Name), rid, rawURL})
	}
	if len(active) == 0 {
		return mediaURLs, mediaTypes, nil
	}
	name := adapter.Name()
	err := gatherResolve(ctx, adapter, active, resolveBatch{
		scope: "media",
		getURL: func(ctx context.Context, rawURL string) (string, error) {
			return resolveDownloadURLFn(ctx, adapter, rawURL)
		},
		failed: func(kind, rawURL string, err error) string {
			return fmt.Sprintf("[%s] inbound media resolve failed: kind=%s url=%s err=%s", name, kind, rawURL, errText(err))
		},
		crashed: func(kind, rawURL string, err error) string {
			return fmt.Sprintf("[%s] inbound media resolve crashed: kind=%s url=%s err=%s", name, kind, pystr.Slice(rawURL, 0, 80), errText(err))
		},
		logTag: func(rawURL string) string { return "placeholder_url=" + pystr.Slice(rawURL, 0, 80) },
	}, &mediaURLs, &mediaTypes)
	return mediaURLs, mediaTypes, err
}

// Local paths and MIME types of the resolvable ybres anchors in input order, each failed resource id skipped
func resolveYbresRefs(ctx context.Context, adapter *YuanbaoAdapter, refs []ybresRef, logPrefix string) ([]string, []string, error) {
	mediaPaths, mimes := []string{}, []string{}
	var active []resolveItem
	for _, ref := range refs {
		if resolvableMediaKinds[ref.kind] && !appendCachedResource(adapter, ref.rid, &mediaPaths, &mimes) {
			active = append(active, resolveItem{ref.kind, ref.filename, ref.rid, ref.rid})
		}
	}
	if len(active) == 0 {
		return mediaPaths, mimes, nil
	}
	name := adapter.Name()
	err := gatherResolve(ctx, adapter, active, resolveBatch{
		scope:  "ybres",
		getURL: func(ctx context.Context, rid string) (string, error) { return fetchResourceURLFn(ctx, adapter, rid) },
		failed: func(kind, rid string, err error) string {
			return fmt.Sprintf("[%s] %s resolve failed: rid=%s kind=%s err=%s", name, logPrefix, rid, kind, errText(err))
		},
		crashed: func(kind, rid string, err error) string {
			return fmt.Sprintf("[%s] %s resolve crashed: rid=%s kind=%s err=%s", name, logPrefix, rid, kind, errText(err))
		},
		logTag: func(rid string) string { return logPrefix + " rid=" + rid },
	}, &mediaPaths, &mimes)
	return mediaPaths, mimes, err
}

// Latest observed image and file anchors of the transcript resolved to local paths in chronological order
func collectObservedMedia(ctx context.Context, adapter *YuanbaoAdapter, source *gateway.SessionSource) ([]string, []string, error) {
	store := sessionStoreOf(adapter)
	if store == nil || source == nil {
		return nil, nil, nil
	}
	var history []any
	entry, err := store.GetOrCreateSession(ctx, source, false, true)
	if err == nil {
		history, err = store.LoadTranscript(ctx, entry.SessionID)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		var unreadable *gateway.TranscriptReadError
		if errors.As(err, &unreadable) {
			slog.Warn(fmt.Sprintf("[%s] Observed-media hydration: transcript unreadable: %s", adapter.Name(), errText(err)))
		} else {
			slog.Warn(fmt.Sprintf("[%s] Observed-media hydration setup failed: %s", adapter.Name(), errText(err)))
		}
		return nil, nil, nil
	}
	var order []ybresRef
	seen := map[string]bool{}
	recent := history[max(0, len(history)-observedMediaBackfillLookback):]
	for i := len(recent) - 1; i >= 0 && len(order) < observedMediaBackfillMaxResolvePerTurn; i-- {
		msg, err := dictOf(recent[i])
		if err != nil {
			return nil, nil, err
		}
		content, isStr := msg["content"].(string)
		if !isStr || !strings.Contains(content, "|ybres:") {
			continue
		}
		matches := findAll(ybResRefRE, content)
		for j, k := 0, len(matches)-1; j < k; j, k = j+1, k-1 {
			matches[j], matches[k] = matches[k], matches[j]
		}
		for _, ref := range iterYbresRefs(matches) {
			if !seen[ref.rid] {
				seen[ref.rid] = true
				order = append(order, ref)
			}
			if len(order) >= observedMediaBackfillMaxResolvePerTurn {
				break
			}
		}
	}
	if len(order) == 0 {
		return nil, nil, nil
	}
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
	}
	return resolveYbresRefs(ctx, adapter, order, "observed-media")
}

// Local paths and MIME types of the ybres anchors of the quoted message
func resolveQuoteMedia(ctx context.Context, adapter *YuanbaoAdapter, quoteMediaRefs []ybresRef) ([]string, []string, error) {
	return resolveYbresRefs(ctx, adapter, quoteMediaRefs, "quote")
}

// Media of a quoted DM message that its own turn already rewrote to local paths, read from the message cache
func collectQuoteLocalMedia(ic *InboundContext) ([]string, []string) {
	var paths, mimes []string
	if ic.ReplyToMessageID == nil || *ic.ReplyToMessageID == "" {
		return paths, mimes
	}
	seen := map[string]bool{}
	for _, m := range findAll(ybLocalMediaRE, ic.Adapter.cachedMsgContent(*ic.ReplyToMessageID)) {
		path := pystr.Strip(m.GroupByNumber(2).String())
		if path == "" || seen[path] {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
		mimes = append(mimes, GuessMIMEType(pypath.Basename(path)))
	}
	return paths, mimes
}

// Merges the media of the message, then of its quote or the group backlog, and stops on a bare placeholder
func (m *MediaResolveMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	adapter := ic.Adapter
	urls, types := []string{}, []string{}
	seen := map[string]bool{}
	addUnique := func(paths, mimes []string) {
		for i := 0; i < len(paths) && i < len(mimes); i++ {
			if paths[i] != "" && !seen[paths[i]] {
				seen[paths[i]] = true
				urls = append(urls, paths[i])
				types = append(types, mimes[i])
			}
		}
	}
	ownPaths, ownMimes, err := resolveMediaURLsFn(ctx, adapter, ic.MediaRefs)
	if err != nil {
		return err
	}
	ownCount := 0
	for _, path := range ownPaths {
		if path != "" {
			ownCount++
		}
	}
	addUnique(ownPaths, ownMimes)
	switch {
	case ic.ReplyToMessageID != nil && len(ic.QuoteMediaRefs) > 0:
		paths, mimes, err := resolveQuoteMediaFn(ctx, adapter, ic.QuoteMediaRefs)
		if err != nil {
			return err
		}
		addUnique(paths, mimes)
	case ic.ReplyToMessageID != nil:
		addUnique(collectQuoteLocalMedia(ic))
	case ic.ChatType == "group":
		paths, mimes, err := collectObservedMediaFn(ctx, adapter, ic.Source)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			slog.Warn(fmt.Sprintf("[%s] observed-image hydration raised, continuing anyway: %s", adapter.Name(), errText(err)))
		default:
			addUnique(paths, mimes)
		}
	}
	ic.MediaURLs, ic.MediaTypes = urls, types
	if isSkippablePlaceholder(ic.RawText, ownCount) {
		slog.Debug(fmt.Sprintf("[%s] Skip placeholder after media download: %s", adapter.Name(), pyrepr.StrRepr(ic.RawText)))
		return nil
	}
	return next()
}

// PatchAnchorsMiddleware rewrites the ybres anchors of the raw text to the local paths the media resolved to
type PatchAnchorsMiddleware struct{ middlewareBase }

func newPatchAnchorsMiddleware() InboundMiddleware {
	return &PatchAnchorsMiddleware{middlewareBase{"PatchAnchorsMiddleware", "patch-anchors"}}
}

// Text with one anchor replaced per resolved absolute path in order, an anchor its media does not fit left untouched
func patchAnchors(text string, urls, types []string) string {
	patched := text
	for i := 0; i < len(urls) && i < len(types); i++ {
		path, mime := urls[i], types[i]
		if !strings.HasPrefix(path, "/") {
			continue
		}
		anchor, err := ybResRefRE.FindStringMatch(patched)
		if err != nil || anchor == nil {
			break
		}
		kind, filename := pystrPartition(anchor.GroupByNumber(1).String(), ":")
		var replacement string
		switch kind = pystr.Strip(kind); {
		case kind == "image" && strings.HasPrefix(mime, "image/"):
			replacement = "[image: " + path + "]"
		case kind == "file":
			label := pystr.Strip(filename)
			if label == "" {
				label = pypath.Basename(path)
			}
			replacement = "[file: " + label + " → " + path + "]"
		case kind == "video":
			replacement = "[video: " + path + "]"
		default:
			continue
		}
		start, length := anchor.ByteRange()
		patched = patched[:start] + replacement + patched[start+length:]
	}
	return patched
}

func (m *PatchAnchorsMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	ic.RawText = patchAnchors(ic.RawText, ic.MediaURLs, ic.MediaTypes)
	return next()
}
