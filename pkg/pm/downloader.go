package pm

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// UserAgent is a browser shaped agent every asset CDN accepts, with identity encoding requested
const UserAgent = "Mozilla/5.0 (X11; Linux x86_64) icebath-pm/1.0"

// DefaultConnections is the parallel range limit of one download
const DefaultConnections = 8

const (
	downloadChunk   = 1 << 20
	releaseWait     = 60 * time.Second
	probeTimeout    = 60 * time.Second
	transferTimeout = 120 * time.Second
)

// Period between durable coverage checkpoints of a running ranged transfer
var checkpointInterval = 5 * time.Second

var loopbackPrefixes = []string{"http://127.0.0.1:", "http://localhost:", "http://[::1]:"}

var fullSHA256 = regexp.MustCompile(`^[a-f0-9]{64}$`)

var contentRangePattern = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+)$`)

// Reports whether a URL is plain http on a loopback listener
func isLoopbackURL(u string) bool {
	for _, p := range loopbackPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// DownloadErrorKind distinguishes the downloader failure classes
type DownloadErrorKind int

const (
	// DownloadFailed is a generic downloader failure
	DownloadFailed DownloadErrorKind = iota
	// DownloadRangeFailed means the range or representation identity changed
	DownloadRangeFailed
	// DownloadHashFailed means the bytes did not match the pinned sha256
	DownloadHashFailed
	// DownloadWasPaused means a pause stopped the transfer with partials intact
	DownloadWasPaused
	// DownloadTransportFailed is an exhausted network request with its status and URL
	DownloadTransportFailed
)

// DownloadError is every downloader failure, the kind naming its class
type DownloadError struct {
	Kind            DownloadErrorKind
	Message         string
	URL             string
	Status          int
	FallbackAllowed bool
	Cause           error
}

func (e *DownloadError) Error() string { return e.Message }

// Builds a downloader failure of a kind
func downloadError(kind DownloadErrorKind, format string, args ...any) *DownloadError {
	return &DownloadError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Reports whether err is a downloader failure of the given kind
func IsDownloadKind(err error, kind DownloadErrorKind) bool {
	var de *DownloadError
	return errors.As(err, &de) && de.Kind == kind
}

// Wraps an exhausted network failure with its URL, status and whether a pinned mirror may take over
func NewDownloadTransportError(rawURL string, cause error) *DownloadError {
	status := 0
	var httpErr *HTTPError
	if errors.As(cause, &httpErr) {
		status = httpErr.Code
	}
	var dnsErr *net.DNSError
	unresolved := errors.As(cause, &dnsErr)
	fallback := status == 401 || status == 403 || status == 404 || status == 410 || status >= 500 || unresolved || IsTransient(cause)
	refused := EdgeBlock(cause)
	if refused == "" && (status == 401 || status == 403) {
		refused = "the host refused access"
	}
	reason := cause.Error()
	if refused != "" {
		reason = fmt.Sprintf("%s; %s", cause, refused)
	}
	return &DownloadError{Kind: DownloadTransportFailed, Message: fmt.Sprintf("download failed from %s: %s", rawURL, reason),
		URL: rawURL, Status: status, FallbackAllowed: fallback, Cause: cause}
}

// Refuses a redirect that leaves https, plain http staying allowed only from loopback to loopback
func httpsRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	next := req.URL.String()
	loopback := isLoopbackURL(via[len(via)-1].URL.String()) && isLoopbackURL(next)
	if !strings.HasPrefix(next, "https://") && !loopback {
		return downloadError(DownloadFailed, "refusing redirect to non-https url: %s", next)
	}
	return nil
}

// HTTP client every PM fetch shares, one HTTP/1.1 connection per range and https held across redirects
var downloadClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DisableCompression:  true,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConnsPerHost: DefaultConnections * 2,
		IdleConnTimeout:     90 * time.Second,
	},
	CheckRedirect: httpsRedirectPolicy,
}

// socketTimeout is a blocking network operation that saw no activity within its timeout
type socketTimeout struct{}

func (socketTimeout) Error() string   { return "timed out" }
func (socketTimeout) Timeout() bool   { return true }
func (socketTimeout) Temporary() bool { return true }

// Cancels a request when no network activity happens within the timeout
type activityWatch struct {
	timer   *time.Timer
	timeout time.Duration
	fired   atomic.Bool
}

// Body that renews the activity watch on every read and reports a stall as a timeout
type watchedBody struct {
	body   io.ReadCloser
	watch  *activityWatch
	cancel context.CancelFunc
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil && b.watch.fired.Load() {
		return n, socketTimeout{}
	}
	b.watch.timer.Reset(b.watch.timeout)
	return n, err
}

func (b *watchedBody) Close() error {
	b.watch.timer.Stop()
	err := b.body.Close()
	b.cancel()
	return err
}

// Opens a URL with PM headers and a per operation timeout, non 2xx statuses failing as HTTP errors
func openURL(rawURL string, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, &invalidURLError{err: err}
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	watch := &activityWatch{timeout: timeout}
	watch.timer = time.AfterFunc(timeout, func() { watch.fired.Store(true); cancel() })
	resp, err := downloadClient.Do(req)
	if err != nil {
		watch.timer.Stop()
		cancel()
		var de *DownloadError
		if errors.As(err, &de) {
			return nil, de
		}
		if watch.fired.Load() {
			return nil, &url.Error{Op: "Get", URL: rawURL, Err: socketTimeout{}}
		}
		return nil, err
	}
	watch.timer.Reset(timeout)
	resp.Body = &watchedBody{body: resp.Body, watch: watch, cancel: cancel}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, httpErrorFrom(rawURL, resp)
	}
	return resp, nil
}

// invalidURLError is a URL the request builder rejects, which Python raises as a ValueError
type invalidURLError struct{ err error }

func (e *invalidURLError) Error() string { return e.err.Error() }

// Reads up to n bytes the way a buffered Python read does, a short final read meaning end of body
func readChunk(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return buf[:got], err
}

// Range is a half open byte interval
type Range struct{ Start, End int64 }

// RangeSet is one destination's covered runs, the last set in a report naming the source that ticked
type RangeSet struct {
	Key    string
	Ranges []Range
}

// ProgressFunc receives overall done and total bytes and every destination's coverage, an error failing the job
type ProgressFunc func(done, total int64, ranges []RangeSet) error

// Checks that a ranged response covers exactly the requested interval and returns its representation size
func validateRange(resp *http.Response, start, end int64, total int64, haveTotal bool) (int64, error) {
	m := contentRangePattern.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if resp.StatusCode != 206 || m == nil {
		return 0, downloadError(DownloadRangeFailed, "server did not honor the requested byte range")
	}
	first, _ := strconv.ParseInt(m[1], 10, 64)
	last, _ := strconv.ParseInt(m[2], 10, 64)
	size, _ := strconv.ParseInt(m[3], 10, 64)
	if first != start || last != end-1 || size < end || (haveTotal && size != total) {
		return 0, downloadError(DownloadRangeFailed, "server returned a different byte range or representation size")
	}
	if values, ok := resp.Header["Content-Length"]; ok && len(values) > 0 {
		length := values[0]
		n, err := strconv.ParseInt(length, 10, 64)
		if !isDecimal(length) || err != nil || n != end-start {
			return 0, downloadError(DownloadRangeFailed, "range Content-Length does not match its bounds")
		}
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && strings.ToLower(encoding) != "identity" {
		return 0, downloadError(DownloadRangeFailed, "encoded response cannot be written into an identity byte range")
	}
	return size, nil
}

// Reports a non empty run of decimal digits
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// What a probe learned about the remote representation
type remoteInfo struct {
	total  int64
	ranged bool
	etag   string
}

// Strong ETag of a response, empty for a weak or missing one
func strongETag(resp *http.Response) string {
	value := resp.Header.Get("ETag")
	if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && len(value) >= 1 {
		return value
	}
	return ""
}

// Source is one destination with its URL, optional pinned sha256 and pinned mirror fallbacks
type Source struct {
	URL       string
	Dest      string
	SHA256    string
	Fallbacks []string
}

// Merges half open ranges into sorted disjoint runs
func coalesce(ranges []Range) []Range {
	var runs []Range
	for _, r := range ranges {
		if r.End > r.Start {
			runs = append(runs, r)
		}
	}
	if len(runs) == 0 {
		return nil
	}
	slices.SortFunc(runs, func(a, b Range) int {
		if a.Start != b.Start {
			return cmpInt64(a.Start, b.Start)
		}
		return cmpInt64(a.End, b.End)
	})
	out := []Range{}
	cur := runs[0]
	for _, r := range runs[1:] {
		if r.Start <= cur.End {
			cur.End = max(cur.End, r.End)
		} else {
			out = append(out, cur)
			cur = r
		}
	}
	return append(out, cur)
}

// Three way comparison of two integers
func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Gaps in zero to total not covered by the coalesced bitmap
func missingRanges(total int64, covered []Range) []Range {
	var missing []Range
	var cursor int64
	for _, r := range coalesce(covered) {
		if r.Start > cursor {
			missing = append(missing, Range{cursor, r.Start})
		}
		cursor = max(cursor, r.End)
	}
	if cursor < total {
		missing = append(missing, Range{cursor, total})
	}
	return missing
}

// Total bytes a coverage list holds
func coveredBytes(ranges []Range) int64 {
	var n int64
	for _, r := range ranges {
		n += r.End - r.Start
	}
	return n
}

// Hex sha256 of a file streamed in chunks
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, downloadChunk)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Rename used to publish downloads, replaceable by tests that simulate a held file
var renameFile = os.Rename

// Renames a finished download into place, riding out a transient hold until the timeout
var ReplaceWhenReleased = func(tmp, dest string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := 100 * time.Millisecond
	for {
		err := renameFile(tmp, dest)
		if err == nil {
			return nil
		}
		if !isAccessError(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return &chainedError{msg: fmt.Sprintf("The download finished, but another program (usually an antivirus scan) kept %s open and it could not be renamed into place. Please try again.", filepath.Base(tmp)), cause: err}
		}
		time.Sleep(delay)
		delay = min(delay*2, 2*time.Second)
	}
}

// Failure with its own message whose cause or errno class stays reachable through errors.Is
type chainedError struct {
	msg   string
	cause error
}

func (e *chainedError) Error() string { return e.msg }

func (e *chainedError) Unwrap() error { return e.cause }

// Reports whether a destination already holds the wanted bytes, pinned files rehashed
func existingDestOK(source Source) (bool, error) {
	st, err := os.Stat(source.Dest)
	if err != nil {
		return false, nil
	}
	if !st.Mode().IsRegular() {
		return false, &chainedError{msg: "download destination is not a file: " + source.Dest, cause: syscall.EISDIR}
	}
	if source.SHA256 == "" {
		return true, nil
	}
	actual, err := sha256File(source.Dest)
	if err != nil {
		return false, nil
	}
	return actual == source.SHA256, nil
}

// PauseEvent is a settable flag a download checks between chunks and while waiting
type PauseEvent struct {
	mu  sync.Mutex
	set bool
	ch  chan struct{}
}

// Fresh unset pause flag
func NewPauseEvent() *PauseEvent { return &PauseEvent{ch: make(chan struct{})} }

// Raises the flag and wakes every waiter
func (e *PauseEvent) Set() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.set {
		e.set = true
		close(e.ch)
	}
}

// Lowers the flag
func (e *PauseEvent) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set {
		e.set = false
		e.ch = make(chan struct{})
	}
}

// Reports whether the flag is raised
func (e *PauseEvent) IsSet() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set
}

// Waits up to d for the flag and reports whether it is raised
func (e *PauseEvent) Wait(d time.Duration) bool {
	e.mu.Lock()
	ch, set := e.ch, e.set
	e.mu.Unlock()
	if set {
		return true
	}
	if d <= 0 {
		return e.IsSet()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return e.IsSet()
	}
}

// DownloadOptions shape a download job
type DownloadOptions struct {
	DisableResume bool
	Connections   int
	PartialsDir   string
	Pause         *PauseEvent
}

// Download is one resumable job, a plan of sources fetched with parallel ranges and verified before publication
type Download struct {
	Sources     []Source
	Resume      bool
	Connections int
	PartialsDir string
	ownsPause   bool
	paused      *PauseEvent
	durableDone atomic.Int64
}

// Plans a download, zero connections taking the default and partials landing in the managed area
func NewDownload(sources []Source, opts DownloadOptions) *Download {
	connections := opts.Connections
	if connections == 0 {
		connections = DefaultConnections
	}
	d := &Download{Resume: !opts.DisableResume, Connections: max(1, connections), PartialsDir: opts.PartialsDir}
	for _, s := range sources {
		d.Sources = append(d.Sources, Source{URL: s.URL, Dest: s.Dest, SHA256: s.SHA256, Fallbacks: slices.Clone(s.Fallbacks)})
	}
	if d.PartialsDir == "" {
		d.PartialsDir = PartialsRoot()
	}
	d.ownsPause = opts.Pause == nil
	d.paused = opts.Pause
	if d.paused == nil {
		d.paused = NewPauseEvent()
	}
	return d
}

// Requests a stop between chunks, Run then failing with a paused error
func (d *Download) Pause() { d.paused.Set() }

// Fetches every source and returns the published destination paths
func (d *Download) Run(progress ProgressFunc) ([]string, error) {
	if d.ownsPause {
		d.paused.Clear()
	}
	if err := d.checkPause(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d.PartialsDir, 0o777); err != nil {
		return nil, err
	}
	for _, source := range d.Sources {
		if len(source.Fallbacks) > 0 && !fullSHA256.MatchString(source.SHA256) {
			return nil, errors.New("mirror fallback requires a full lowercase SHA256")
		}
		for _, u := range append([]string{source.URL}, source.Fallbacks...) {
			if !strings.HasPrefix(u, "https://") && !isLoopbackURL(u) {
				return nil, fmt.Errorf("refusing non-https url: %s", u)
			}
		}
	}
	totals := map[string]int64{}
	unknown := map[string]bool{}
	var coverage []RangeSet
	setCoverage := func(key string, rows []Range) {
		for i, rs := range coverage {
			if rs.Key == key {
				coverage = append(coverage[:i], coverage[i+1:]...)
				break
			}
		}
		coverage = append(coverage, RangeSet{Key: key, Ranges: slices.Clone(rows)})
	}
	var selected []Source
	failures := map[string]*[]*DownloadError{}
	for _, source := range d.Sources {
		if err := d.checkPause(); err != nil {
			return nil, err
		}
		key := source.Dest
		failures[key] = &[]*DownloadError{}
		ok, err := existingDestOK(source)
		if err != nil {
			return nil, err
		}
		var remote remoteInfo
		var covered []Range
		if ok {
			st, err := os.Stat(source.Dest)
			if err != nil {
				return nil, err
			}
			remote = remoteInfo{total: st.Size()}
			covered = []Range{{0, remote.total}}
		} else {
			var candidate Source
			candidate, err = d.trySources(source, func(c Source) error {
				r, err := d.probe(c.URL)
				remote = r
				return err
			}, failures[key])
			if err != nil {
				return nil, err
			}
			source = candidate
			release, acquired, err := PartialLock(d.PartialsDir, downloadKey(d, source.URL), nil, false)
			if err != nil {
				return nil, err
			}
			if acquired && remote.ranged {
				covered = d.partialRanges(source, remote)
			}
			release()
			if remote.total == 0 {
				unknown[key] = true
			}
		}
		selected = append(selected, source)
		totals[key] = remote.total
		setCoverage(key, covered)
	}
	report := func(key string, written []Range, total int64, complete bool) error {
		totals[key] = total
		if total != 0 || complete {
			delete(unknown, key)
		} else {
			unknown[key] = true
		}
		setCoverage(key, written)
		if progress != nil {
			var done, sum int64
			for _, rs := range coverage {
				done += coveredBytes(rs.Ranges)
			}
			if len(unknown) == 0 {
				for _, t := range totals {
					sum += t
				}
			}
			snapshot := make([]RangeSet, len(coverage))
			for i, rs := range coverage {
				snapshot[i] = RangeSet{Key: rs.Key, Ranges: slices.Clone(rs.Ranges)}
			}
			return progress(done, sum, snapshot)
		}
		return nil
	}
	for _, source := range selected {
		key := source.Dest
		tick := func(written []Range, total int64) error { return report(key, written, total, false) }
		var size int64
		if _, err := d.trySources(source, func(c Source) error {
			n, err := d.transfer(c, tick)
			size = n
			return err
		}, failures[key]); err != nil {
			return nil, err
		}
		if err := report(key, []Range{{0, size}}, size, true); err != nil {
			return nil, err
		}
	}
	out := make([]string, len(d.Sources))
	for i, s := range d.Sources {
		out[i] = s.Dest
	}
	return out, nil
}

// Runs an operation against the source then each pinned mirror while failures allow falling back
func (d *Download) trySources(source Source, operation func(Source) error, failures *[]*DownloadError) (Source, error) {
	var urls []string
	for _, u := range append([]string{source.URL}, source.Fallbacks...) {
		if !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
	}
	for index, u := range urls {
		if err := d.checkPause(); err != nil {
			return Source{}, err
		}
		candidate := Source{URL: u, Dest: source.Dest, SHA256: source.SHA256, Fallbacks: slices.Clone(urls[index+1:])}
		err := operation(candidate)
		if err == nil {
			return candidate, nil
		}
		var de *DownloadError
		if !errors.As(err, &de) || de.Kind != DownloadTransportFailed {
			return Source{}, err
		}
		*failures = append(*failures, de)
		if !de.FallbackAllowed || index == len(urls)-1 {
			if len(*failures) == 1 {
				return Source{}, de
			}
			messages := make([]string, len(*failures))
			for i, f := range *failures {
				messages[i] = f.Error()
			}
			return Source{}, &DownloadError{Kind: DownloadFailed, Message: strings.Join(messages, "\n"), Cause: de}
		}
		slog.Debug(fmt.Sprintf("%s; trying pinned mirror %s", de, urls[index+1]))
	}
	return Source{}, downloadError(DownloadFailed, "no download url for %s", source.Dest)
}

// Transfers one source under its partial's ownership lock and publishes it after verification
func (d *Download) transfer(source Source, tick func([]Range, int64) error) (int64, error) {
	release, acquired, err := PartialLock(d.PartialsDir, downloadKey(d, source.URL), d.paused.IsSet, true)
	if err != nil {
		return 0, err
	}
	defer release()
	if !acquired {
		return 0, downloadError(DownloadWasPaused, "%s", source.URL)
	}
	if err := d.checkPause(); err != nil {
		return 0, err
	}
	ok, err := existingDestOK(source)
	if err != nil {
		return 0, err
	}
	if ok {
		st, err := os.Stat(source.Dest)
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	}
	remote, err := d.probe(source.URL)
	if err != nil {
		return 0, err
	}
	var covered []Range
	if remote.ranged {
		covered = d.partialRanges(source, remote)
	}
	d.durableDone.Store(coveredBytes(covered))
	if err := tick(covered, remote.total); err != nil {
		return 0, err
	}
	resumable := remote.total != 0 && remote.ranged && (source.SHA256 != "" || remote.etag != "")
	report := func(written []Range) error { return tick(written, remote.total) }
	fetch := func() (int64, error) {
		if err := d.checkPause(); err != nil {
			return 0, err
		}
		if resumable {
			return d.fetchRanged(source, remote, report)
		}
		return d.fetchSingle(source, remote, report)
	}
	configured := d.Connections
	opts := RetryOptions{Wait: func(delay time.Duration) error { return downloadWaitRetry(d, delay) }, FallbackAvailable: len(source.Fallbacks) > 0}
	if d.Resume && resumable {
		opts.Progress = d.durableDone.Load
	}
	size, err := RetryNetwork(fetch, opts)
	d.Connections = configured
	if err != nil {
		var urlErr *url.Error
		var httpErr *HTTPError
		if errors.As(err, &urlErr) || errors.As(err, &httpErr) || (IsTransient(err) && !isDownloadError(err)) {
			return 0, NewDownloadTransportError(source.URL, err)
		}
		return 0, err
	}
	key := downloadKey(d, source.URL)
	if err := d.finalize(source, filepath.Join(d.PartialsDir, key+".part"), filepath.Join(d.PartialsDir, key+".ranges")); err != nil {
		return 0, err
	}
	return size, nil
}

// Reports whether err is one of the downloader's own failures rather than a transport error
func isDownloadError(err error) bool {
	var de *DownloadError
	return errors.As(err, &de)
}

// Backoff wait of a download, replaceable by tests that observe or skip retry delays
var downloadWaitRetry = (*Download).waitRetry

// Single range reader of a download, replaceable by tests that drive the coordinator directly
var downloadReadRange = (*Download).readRange

// Partial key of a download URL, replaceable by tests that observe the ownership wait
var downloadKey = (*Download).key

// Fails with a paused error once a pause was requested
func (d *Download) checkPause() error {
	if d.paused.IsSet() {
		return downloadError(DownloadWasPaused, "download paused")
	}
	return nil
}

// Backoff wait that a pause interrupts
func (d *Download) waitRetry(delay time.Duration) error {
	if d.paused.Wait(delay) {
		return downloadError(DownloadWasPaused, "download paused during retry backoff")
	}
	return nil
}

// Learns the remote length, range support and strong validator with a one byte range request
func (d *Download) probe(rawURL string) (remoteInfo, error) {
	request := func() (remoteInfo, error) {
		if err := d.checkPause(); err != nil {
			return remoteInfo{}, err
		}
		resp, err := openURL(rawURL, map[string]string{"Range": "bytes=0-0"}, probeTimeout)
		if err != nil {
			return remoteInfo{}, err
		}
		defer resp.Body.Close()
		etag := strongETag(resp)
		if resp.StatusCode == 206 {
			total, err := validateRange(resp, 0, 1, 0, false)
			if err != nil {
				return remoteInfo{}, err
			}
			body, err := readChunk(resp.Body, 2)
			if err != nil {
				return remoteInfo{}, err
			}
			if len(body) == 0 {
				return remoteInfo{}, &IncompleteReadError{Partial: 0, Expected: 1}
			}
			if len(body) != 1 {
				return remoteInfo{}, downloadError(DownloadRangeFailed, "range probe returned the wrong byte count")
			}
			return remoteInfo{total: total, ranged: true, etag: etag}, nil
		}
		if resp.StatusCode != 200 {
			return remoteInfo{}, downloadError(DownloadFailed, "unexpected download probe status: %d", resp.StatusCode)
		}
		length := strings.TrimSpace(resp.Header.Get("Content-Length"))
		var total int64
		if length != "" {
			n, err := strconv.ParseInt(length, 10, 64)
			if err != nil {
				return remoteInfo{}, errInvalidLength
			}
			total = n
		}
		return remoteInfo{total: total, etag: etag}, nil
	}
	remote, err := RetryNetwork(request, RetryOptions{Wait: func(delay time.Duration) error { return downloadWaitRetry(d, delay) }})
	if err == nil {
		return remote, nil
	}
	var invalid *invalidURLError
	if errors.Is(err, errInvalidLength) || errors.As(err, &invalid) {
		return remoteInfo{}, nil
	}
	if isDownloadError(err) {
		return remoteInfo{}, err
	}
	return remoteInfo{}, NewDownloadTransportError(rawURL, err)
}

// A Content-Length that does not parse, which leaves the remote size unknown
var errInvalidLength = errors.New("invalid Content-Length")

// Partial key of a URL
func (d *Download) key(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// Durable ranges of a partial still bound to the same length, validator and pin, empty otherwise
func (d *Download) loadSidecar(side, part string, remote remoteInfo, sha string) []Range {
	if !d.Resume || (sha == "" && remote.etag == "") {
		return nil
	}
	data, err := os.ReadFile(side)
	if err != nil {
		return nil
	}
	v, err := loadJSON(data)
	if err != nil {
		return nil
	}
	doc, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	total, hasTotal := doc["total"]
	etag, hasETag := doc["etag"]
	recorded, hasSHA := doc["sha256"]
	rows, hasRanges := doc["ranges"]
	if !hasTotal || !hasETag || !hasSHA || !hasRanges {
		return nil
	}
	if !jsonEquals(total, remote.total) || !jsonEquals(etag, remote.etag) || !jsonEquals(recorded, sha) {
		return nil
	}
	st, err := os.Stat(part)
	if err != nil {
		return nil
	}
	limit := min(st.Size(), remote.total)
	list, ok := rows.([]any)
	if !ok {
		return nil
	}
	var ranges []Range
	for _, row := range list {
		pair, ok := row.([]any)
		if !ok || len(pair) != 2 {
			return nil
		}
		a, okA := jsonStrictInt(pair[0])
		b, okB := jsonStrictInt(pair[1])
		if !okA || !okB || !(0 <= a && a < b && b <= limit) {
			return nil
		}
		ranges = append(ranges, Range{a, b})
	}
	return coalesce(ranges)
}

// Integer value of a decoded JSON number written without fraction or exponent
func jsonStrictInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(n.String(), ".eE") {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	return i, err == nil
}

// Durable ranges of a source's partial
func (d *Download) partialRanges(source Source, remote remoteInfo) []Range {
	key := downloadKey(d, source.URL)
	return d.loadSidecar(filepath.Join(d.PartialsDir, key+".ranges"), filepath.Join(d.PartialsDir, key+".part"), remote, source.SHA256)
}

// Fsyncs the partial then atomically publishes the durable coverage snapshot beside it
func writeSidecar(side, part string, covered []Range, remote remoteInfo, sha string) error {
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	rows := make([][2]int64, len(covered))
	for i, r := range covered {
		rows[i] = [2]int64{r.Start, r.End}
	}
	body, err := dumpsJSON(struct {
		Total  int64      `json:"total"`
		ETag   string     `json:"etag"`
		SHA256 string     `json:"sha256"`
		Ranges [][2]int64 `json:"ranges"`
	}{remote.total, remote.etag, sha, rows}, false)
	if err != nil {
		return err
	}
	return RetryHeld(func() error { return durableWrite(side, body) })
}

// Durable publisher of sidecar bytes, replaceable by tests that refuse a checkpoint
var durableWrite = DurableWriteBytes

// Fetches the missing ranges of a resumable source in parallel until the partial is complete
func (d *Download) fetchRanged(source Source, remote remoteInfo, tick func([]Range) error) (int64, error) {
	key := downloadKey(d, source.URL)
	part := filepath.Join(d.PartialsDir, key+".part")
	side := filepath.Join(d.PartialsDir, key+".ranges")
	covered := d.loadSidecar(side, part, remote, source.SHA256)
	flags := os.O_RDWR
	if len(covered) == 0 {
		flags |= os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o666)
	if err != nil {
		return 0, err
	}
	if err := f.Truncate(remote.total); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	for {
		gapCount := (remote.total + downloadChunk - 1) / downloadChunk
		if len(covered) > 0 {
			gapCount = int64(len(missingRanges(remote.total, covered)))
		}
		connections := max(1, min(int64(d.Connections), gapCount))
		var errs []error
		covered, errs = d.rangedAttempt(source, remote, part, covered, int(connections), tick)
		for _, e := range errs {
			if IsDownloadKind(e, DownloadRangeFailed) {
				os.Remove(part)
				os.Remove(side)
				return 0, e
			}
		}
		if err := writeSidecar(side, part, covered, remote, source.SHA256); err != nil {
			return 0, err
		}
		d.durableDone.Store(coveredBytes(covered))
		if d.paused.IsSet() {
			return 0, downloadError(DownloadWasPaused, "%s", source.URL)
		}
		if len(errs) > 0 {
			if d.Connections > 1 && allRefusedRanges(errs) {
				slog.Warn(fmt.Sprintf("parallel range fetch refused for %s; retrying with one connection", source.URL))
				d.Connections = 1
				continue
			}
			for _, e := range errs {
				if !IsTransient(e) {
					return 0, e
				}
			}
			return 0, errs[0]
		}
		written := d.durableDone.Load()
		if written != remote.total {
			return 0, &IncompleteReadError{Partial: 0, Expected: remote.total - written}
		}
		return written, nil
	}
}

// Reports whether every range failure is a 403 or 404 refusal
func allRefusedRanges(errs []error) bool {
	for _, e := range errs {
		var httpErr *HTTPError
		if !errors.As(e, &httpErr) || (httpErr.Code != 403 && httpErr.Code != 404) {
			return false
		}
	}
	return true
}

// Streams one byte range into the partial, recording durable progress after every chunk
func (d *Download) readRange(source Source, remote remoteInfo, part string, start, end int64, stop *PauseEvent, record func(int64, int64)) error {
	if d.paused.IsSet() || stop.IsSet() {
		return nil
	}
	headers := map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end-1)}
	if remote.etag != "" {
		headers["If-Range"] = remote.etag
	}
	resp, err := openURL(source.URL, headers, transferTimeout)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := validateRange(resp, start, end, remote.total, true); err != nil {
		return err
	}
	if remote.etag != "" && strongETag(resp) != remote.etag {
		return downloadError(DownloadRangeFailed, "remote representation changed during download")
	}
	position := start
	for position < end {
		if d.paused.IsSet() || stop.IsSet() {
			return nil
		}
		chunk, err := readChunk(resp.Body, int(min(downloadChunk, end-position)))
		if err != nil {
			return err
		}
		if len(chunk) == 0 {
			return &IncompleteReadError{Partial: 0, Expected: end - position}
		}
		if _, err := f.WriteAt(chunk, position); err != nil {
			return err
		}
		position += int64(len(chunk))
		if position == end {
			extra, err := readChunk(resp.Body, 1)
			if err != nil {
				return err
			}
			if len(extra) > 0 {
				return downloadError(DownloadRangeFailed, "range body exceeds its declared bounds")
			}
		}
		record(start, position)
	}
	return nil
}

// One parallel pass over the missing ranges, checkpointing and reporting coverage from the coordinator
func (d *Download) rangedAttempt(source Source, remote remoteInfo, part string, covered []Range, connections int, tick func([]Range) error) ([]Range, []error) {
	ranges := missingRanges(remote.total, covered)
	if len(covered) == 0 {
		count := max(1, min(int64(connections), (remote.total+downloadChunk-1)/downloadChunk))
		ranges = nil
		for i := int64(0); i < count; i++ {
			ranges = append(ranges, Range{i * remote.total / count, (i + 1) * remote.total / count})
		}
	}
	var mu sync.Mutex
	var errs []error
	stop := NewPauseEvent()
	updated := make(chan struct{}, 1)
	signal := func() {
		select {
		case updated <- struct{}{}:
		default:
		}
	}
	pending := len(ranges)
	covered = slices.Clone(covered)
	reported := slices.Clone(covered)
	checkpointed := slices.Clone(covered)
	nextCheckpoint := time.Now().Add(checkpointInterval)
	record := func(start, position int64) {
		mu.Lock()
		covered = coalesce(append(slices.Clone(covered), Range{start, position}))
		mu.Unlock()
		signal()
	}
	slots := make(chan struct{}, connections)
	var wg sync.WaitGroup
	for _, r := range ranges {
		wg.Add(1)
		go func(start, end int64) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			err := downloadReadRange(d, source, remote, part, start, end, stop, record)
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			}
			pending--
			mu.Unlock()
			if err != nil {
				stop.Set()
			}
			signal()
		}(r.Start, r.End)
	}
	observerFailed := false
	finished := len(ranges) == 0
	for !finished {
		wait := time.Until(nextCheckpoint)
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-updated:
			case <-t.C:
			}
			t.Stop()
		} else {
			select {
			case <-updated:
			default:
			}
		}
		mu.Lock()
		snapshot := slices.Clone(covered)
		finished = pending == 0
		mu.Unlock()
		if !time.Now().Before(nextCheckpoint) {
			if !slices.Equal(snapshot, checkpointed) {
				if err := writeSidecar(part[:len(part)-len(filepath.Ext(part))]+".ranges", part, snapshot, remote, source.SHA256); err != nil {
					slog.Warn(fmt.Sprintf("could not save download progress for %s; continuing: %s", filepath.Base(source.Dest), err))
				} else {
					d.durableDone.Store(coveredBytes(snapshot))
					checkpointed = snapshot
				}
			}
			nextCheckpoint = time.Now().Add(checkpointInterval)
		}
		if !observerFailed && !slices.Equal(snapshot, reported) {
			if err := tick(snapshot); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				observerFailed = true
				stop.Set()
			}
			reported = snapshot
		}
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	return covered, errs
}

// Streams a whole body when no strong validator allows reusing earlier bytes
func (d *Download) fetchSingle(source Source, remote remoteInfo, tick func([]Range) error) (written int64, err error) {
	key := downloadKey(d, source.URL)
	part := filepath.Join(d.PartialsDir, key+".part")
	side := filepath.Join(d.PartialsDir, key+".ranges")
	var covered []Range
	defer func() {
		if err == nil {
			err = writeSidecar(side, part, covered, remote, source.SHA256)
			return
		}
		if _, statErr := os.Stat(part); statErr == nil {
			if serr := writeSidecar(side, part, covered, remote, source.SHA256); serr != nil {
				err = serr
			}
		}
	}()
	resp, err := openURL(source.URL, nil, transferTimeout)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if resp.StatusCode != 200 {
		return 0, downloadError(DownloadFailed, "unexpected download status: %d", resp.StatusCode)
	}
	if remote.etag != "" && strongETag(resp) != remote.etag {
		return 0, downloadError(DownloadRangeFailed, "remote representation changed during download")
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && strings.ToLower(encoding) != "identity" {
		return 0, downloadError(DownloadFailed, "encoded response cannot be used as an identity download")
	}
	var declared int64
	if length := strings.TrimSpace(resp.Header.Get("Content-Length")); length != "" {
		n, perr := strconv.ParseInt(length, 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.StrRepr(length))
		}
		declared = n
	}
	var position int64
	for {
		if d.paused.IsSet() {
			return 0, downloadError(DownloadWasPaused, "%s", source.URL)
		}
		chunk, rerr := readChunk(resp.Body, downloadChunk)
		if rerr != nil {
			return 0, rerr
		}
		if len(chunk) == 0 {
			break
		}
		if _, werr := f.Write(chunk); werr != nil {
			return 0, werr
		}
		position += int64(len(chunk))
		covered = []Range{{0, position}}
		if terr := tick(slices.Clone(covered)); terr != nil {
			return 0, terr
		}
	}
	if d.paused.IsSet() {
		return 0, downloadError(DownloadWasPaused, "%s", source.URL)
	}
	expected := declared
	if expected == 0 {
		expected = remote.total
	}
	if position < expected {
		return 0, &IncompleteReadError{Partial: 0, Expected: expected - position}
	}
	if (declared != 0 && position != declared) || (remote.total != 0 && position != remote.total) {
		return 0, downloadError(DownloadFailed, "download incomplete (%d bytes, expected %d)", position, expected)
	}
	return position, nil
}

// Verifies the pin, copies the partial beside the destination durably and swaps it into place
func (d *Download) finalize(source Source, part, side string) error {
	if err := d.checkPause(); err != nil {
		return err
	}
	if source.SHA256 != "" {
		actual, err := sha256File(part)
		if err != nil {
			return err
		}
		if actual != source.SHA256 {
			if err := os.Remove(part); err == nil || os.IsNotExist(err) {
				os.Remove(side)
			}
			return downloadError(DownloadHashFailed, "sha256 mismatch for %s: pinned %s, got %s", source.URL, source.SHA256, actual)
		}
	}
	if err := d.checkPause(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(source.Dest), 0o777); err != nil {
		return err
	}
	target, err := os.CreateTemp(filepath.Dir(source.Dest), "."+filepath.Base(source.Dest)+"-*.download")
	if err != nil {
		return err
	}
	staged := target.Name()
	defer os.Remove(staged)
	if err := copyDurably(target, part); err != nil {
		return err
	}
	stagedInfo, err := os.Stat(staged)
	if err != nil {
		return err
	}
	partInfo, err := os.Stat(part)
	if err != nil {
		return err
	}
	if stagedInfo.Size() != partInfo.Size() {
		return downloadError(DownloadFailed, "destination copy did not preserve the complete download")
	}
	if err := d.checkPause(); err != nil {
		return err
	}
	if err := ReplaceWhenReleased(staged, source.Dest, releaseWait); err != nil {
		return err
	}
	if err := os.Remove(part); err == nil || os.IsNotExist(err) {
		os.Remove(side)
	}
	return nil
}

// Copies a file into an open target, fsyncs it and closes it
func copyDurably(target *os.File, source string) error {
	in, err := os.Open(source)
	if err != nil {
		target.Close()
		return err
	}
	defer in.Close()
	if err := copyFileObj(target, in); err != nil {
		target.Close()
		return err
	}
	if err := target.Sync(); err != nil {
		target.Close()
		return err
	}
	return target.Close()
}

// Streams one file object into another in download sized blocks, replaceable by tests that fail a copy
var copyFileObj = func(dst io.Writer, src io.Reader) error {
	_, err := io.CopyBuffer(dst, src, make([]byte, downloadChunk))
	return err
}
