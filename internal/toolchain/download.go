package toolchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Wire and pacing constants of the downloader
const (
	downloadUserAgent  = "Mozilla/5.0 (X11; Linux x86_64) icebath-pm/1.0"
	downloadChunk      = 1 << 20
	checkpointInterval = 5 * time.Second
	defaultConnections = 8
	releaseWait        = 60 * time.Second
	partialGrace       = 6 * time.Hour
	defaultSizeLimit   = 8 << 30
	maxRedirects       = 10
)

// Shape of the Content-Range header of a satisfied byte range
var contentRange = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+)$`)

// One file to fetch into Dest, checked against SHA256 when set, sized by Size or capped by Limit
type Source struct {
	URL       string
	Dest      string
	SHA256    string
	Fallbacks []string
	Size      int64
	Limit     int64
}

// Half open run of bytes of a download that is already on disk
type Range struct {
	Start int64
	End   int64
}

// Download progress of a whole plan, total zero while a size is unknown, ranges by destination
type downloadTick func(done, total int64, active string, ranges map[string][]Range)

// Resumable multi connection fetcher that keeps partial bytes and their state in one directory
type downloader struct {
	client      *http.Client
	partials    string
	connections int
	mirrorHost  string
}

// Size, range support and strong validator of a remote file
type remoteFile struct {
	total  int64
	ranged bool
	etag   string
}

// Redirect a download refuses to follow
type redirectError struct{ to string }

// Target the redirect pointed at
func (e *redirectError) Error() string { return "refusing redirect to non-https url: " + e.to }

// Redirect check keeping every hop on https, loopback tests staying on loopback, within the hop limit
func downloadRedirects(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	from, to := via[len(via)-1].URL.String(), req.URL.String()
	if !strings.HasPrefix(to, "https://") && !(loopbackURL(from) && loopbackURL(to)) {
		return &redirectError{to: to}
	}
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
		req.Header.Del(name)
	}
	return nil
}

// Sorted disjoint runs covering the same bytes as ranges
func coalesce(ranges []Range) []Range {
	runs := make([]Range, 0, len(ranges))
	for _, r := range ranges {
		if r.End > r.Start {
			runs = append(runs, r)
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].Start < runs[j].Start || (runs[i].Start == runs[j].Start && runs[i].End < runs[j].End)
	})
	out := runs[:0]
	for _, r := range runs {
		if n := len(out); n > 0 && r.Start <= out[n-1].End {
			out[n-1].End = max(out[n-1].End, r.End)
			continue
		}
		out = append(out, r)
	}
	return out
}

// Gaps of the first total bytes that covered leaves open
func missing(total int64, covered []Range) []Range {
	var gaps []Range
	cursor := int64(0)
	for _, r := range coalesce(slices.Clone(covered)) {
		if r.Start > cursor {
			gaps = append(gaps, Range{cursor, r.Start})
		}
		cursor = max(cursor, r.End)
	}
	if cursor < total {
		gaps = append(gaps, Range{cursor, total})
	}
	return gaps
}

// Bytes the runs hold together
func rangeBytes(ranges []Range) int64 {
	var n int64
	for _, r := range ranges {
		n += r.End - r.Start
	}
	return n
}

// Lower case hex sha256 of the file at path
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	if err := errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Name partial bytes of an address are kept under
func partialKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// Partial data file and range state file of an address
func (d *downloader) partPaths(rawURL string) (string, string) {
	key := partialKey(rawURL)
	return filepath.Join(d.partials, key+".part"), filepath.Join(d.partials, key+".ranges")
}

// Lock file that makes one process the owner of the partial of an address
func (d *downloader) lockPath(rawURL string) string {
	return filepath.Join(d.partials, ".locks", partialKey(rawURL))
}

// Reports a destination that already holds the wanted bytes, pinned ones rehashed and unpinned ones accepted
func existingDestOK(src Source) (bool, error) {
	fi, err := os.Stat(src.Dest)
	if err != nil {
		return false, nil
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("download destination is not a file: %s", src.Dest)
	}
	if src.SHA256 == "" {
		return true, nil
	}
	got, err := sha256File(src.Dest)
	return err == nil && got == src.SHA256, nil
}

// Largest body the downloader accepts for src
func limitFor(src Source) int64 {
	switch {
	case src.Limit > 0:
		return src.Limit
	case src.Size > 0:
		return src.Size
	}
	return defaultSizeLimit
}

// Refuses a remote size that differs from the pinned size or passes the limit
func checkSize(src Source, total int64) error {
	if src.Size > 0 && total > 0 && total != src.Size {
		return &SizeError{URL: src.URL, Size: total, Limit: src.Size, Exact: true}
	}
	if limit := limitFor(src); total > limit {
		return &SizeError{URL: src.URL, Size: total, Limit: limit}
	}
	return nil
}

// Strong entity tag of a response, empty for a weak or missing one
func strongETag(resp *http.Response) string {
	v := resp.Header.Get("ETag")
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		return v
	}
	return ""
}

// Reports a response whose body is not the identity bytes of the file
func encoded(resp *http.Response) bool {
	ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	return resp.Uncompressed || (ce != "" && ce != "identity")
}

// Representation size of a 206 response that carries exactly the bytes from start up to end
func validateRange(resp *http.Response, start, end, total int64) (int64, error) {
	m := contentRange.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if resp.StatusCode != http.StatusPartialContent || m == nil {
		return 0, &rangeError{"server did not honor the requested byte range"}
	}
	first, errA := strconv.ParseInt(m[1], 10, 64)
	last, errB := strconv.ParseInt(m[2], 10, 64)
	size, errC := strconv.ParseInt(m[3], 10, 64)
	if errA != nil || errB != nil || errC != nil || first != start || last != end-1 || size < end || (total >= 0 && size != total) {
		return 0, &rangeError{"server returned a different byte range or representation size"}
	}
	if length := resp.Header.Get("Content-Length"); length != "" {
		if n, err := strconv.ParseInt(length, 10, 64); err != nil || n != end-start {
			return 0, &rangeError{"range Content-Length does not match its bounds"}
		}
	}
	if encoded(resp) {
		return 0, &rangeError{"encoded response cannot be written into an identity byte range"}
	}
	return size, nil
}

// Sends one GET with the download headers, a failing status returned as a statusError
func (d *downloader) get(ctx context.Context, rawURL string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", downloadUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return nil, &statusError{url: rawURL, code: resp.StatusCode, status: resp.Status, header: resp.Header}
	}
	return resp, nil
}

// Reports a failure of the request itself rather than of the local disk or the plan
func networkFailure(err error) bool {
	var st *statusError
	var ue *url.Error
	var redirect *redirectError
	if errors.As(err, &redirect) {
		return false
	}
	return errors.As(err, &st) || errors.As(err, &ue) || isTransient(err)
}

// Learns the size, range support and validator of src with a one byte range request
func (d *downloader) probe(ctx context.Context, src Source) (remoteFile, error) {
	var out remoteFile
	err := retryNetwork(ctx, func() error {
		resp, err := d.get(ctx, src.URL, map[string]string{"Range": "bytes=0-0"})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		etag := strongETag(resp)
		switch resp.StatusCode {
		case http.StatusPartialContent:
			total, err := validateRange(resp, 0, 1, -1)
			if err != nil {
				return err
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 2))
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return errIncomplete
			}
			if len(body) != 1 {
				return &rangeError{"range probe returned the wrong byte count"}
			}
			out = remoteFile{total: total, ranged: true, etag: etag}
		case http.StatusOK:
			out = remoteFile{total: max(resp.ContentLength, 0), etag: etag}
		default:
			return fmt.Errorf("unexpected download probe status: %d", resp.StatusCode)
		}
		return nil
	}, nil, false)
	switch {
	case err == nil:
		return out, checkSize(src, out.total)
	case ctx.Err() != nil:
		return out, ctx.Err()
	case networkFailure(err):
		return out, transportFailure(src.URL, err, d.mirrorHost)
	}
	return out, err
}

// State of a partial as kept beside it between runs
type sidecar struct {
	Total  int64     `json:"total"`
	ETag   string    `json:"etag"`
	SHA256 string    `json:"sha256"`
	Ranges [][]int64 `json:"ranges"`
}

// Durable ranges of the partial of src when its state still describes the remote file, none otherwise
func (d *downloader) loadSidecar(src Source, remote remoteFile) []Range {
	if src.SHA256 == "" && remote.etag == "" {
		return nil
	}
	part, side := d.partPaths(src.URL)
	data, err := os.ReadFile(side)
	if err != nil {
		return nil
	}
	var state sidecar
	if json.Unmarshal(data, &state) != nil || state.Total != remote.total || state.ETag != remote.etag || state.SHA256 != src.SHA256 {
		return nil
	}
	fi, err := os.Stat(part)
	if err != nil {
		return nil
	}
	limit := min(fi.Size(), remote.total)
	ranges := make([]Range, 0, len(state.Ranges))
	for _, row := range state.Ranges {
		if len(row) != 2 || row[0] < 0 || row[0] >= row[1] || row[1] > limit {
			return nil
		}
		ranges = append(ranges, Range{row[0], row[1]})
	}
	return coalesce(ranges)
}

// Flushes the partial and atomically publishes the ranges it durably holds
func (d *downloader) writeSidecar(src Source, remote remoteFile, covered []Range) error {
	part, side := d.partPaths(src.URL)
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	state := sidecar{Total: remote.total, ETag: remote.etag, SHA256: src.SHA256, Ranges: make([][]int64, 0, len(covered))}
	for _, r := range covered {
		state.Ranges = append(state.Ranges, []int64{r.Start, r.End})
	}
	data, err := fileutil.EncodeJSON(state, fileutil.JSONStyle{})
	if err != nil {
		return err
	}
	return fileutil.WriteFile(context.Background(), side, data, fileutil.SyncDir(), fileutil.TempPrefix(".publish-"))
}

// Runs op against src and then each fallback a failed transport allows, collecting every failure
func (d *downloader) trySources(ctx context.Context, src Source, failures *[]error, op func(Source) error) (Source, error) {
	var urls []string
	for _, u := range append([]string{src.URL}, src.Fallbacks...) {
		if !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
	}
	for i, u := range urls {
		if err := ctx.Err(); err != nil {
			return Source{}, err
		}
		candidate := src
		candidate.URL, candidate.Fallbacks = u, urls[i+1:]
		err := op(candidate)
		if err == nil {
			return candidate, nil
		}
		var te *TransportError
		if !errors.As(err, &te) {
			return Source{}, err
		}
		*failures = append(*failures, te)
		if !te.fallbackAllowed() || i == len(urls)-1 {
			if len(*failures) == 1 {
				return Source{}, err
			}
			return Source{}, errors.Join(*failures...)
		}
		slog.Debug(fmt.Sprintf("%v; trying pinned mirror %s", te, urls[i+1]))
	}
	return Source{}, fmt.Errorf("download of %s has no address", src.Dest)
}

// Fetches every source into its destination, resuming partials and reporting the whole plan as one job
func (d *downloader) run(ctx context.Context, sources []Source, tick downloadTick) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(d.partials, ".locks"), 0o777); err != nil {
		return err
	}
	for _, src := range sources {
		if len(src.Fallbacks) > 0 && !fullSHA256.MatchString(src.SHA256) {
			return errors.New("mirror fallback requires a full lowercase SHA256")
		}
		for _, u := range append([]string{src.URL}, src.Fallbacks...) {
			if !secureURL(u) {
				return fmt.Errorf("refusing non-https url: %s", u)
			}
		}
	}
	totals := map[string]int64{}
	unknown := map[string]bool{}
	coverage := map[string][]Range{}
	failures := map[string]*[]error{}
	selected := make([]Source, len(sources))
	for i, src := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		failures[src.Dest] = new([]error)
		ok, err := existingDestOK(src)
		if err != nil {
			return err
		}
		if ok {
			fi, err := os.Stat(src.Dest)
			if err != nil {
				return err
			}
			selected[i], totals[src.Dest], coverage[src.Dest] = src, fi.Size(), []Range{{0, fi.Size()}}
			continue
		}
		var remote remoteFile
		chosen, err := d.trySources(ctx, src, failures[src.Dest], func(c Source) error {
			r, err := d.probe(ctx, c)
			remote = r
			return err
		})
		if err != nil {
			return err
		}
		var covered []Range
		if remote.ranged {
			if l, err := fileutil.TryOpenLock(d.lockPath(chosen.URL), fileutil.LockPerm(0o600)); err == nil {
				covered = d.loadSidecar(chosen, remote)
				_ = l.Release()
			}
		}
		if remote.total == 0 {
			unknown[src.Dest] = true
		}
		selected[i], totals[src.Dest], coverage[src.Dest] = chosen, remote.total, covered
	}
	report := func(key string, written []Range, total int64, complete bool) {
		totals[key] = total
		if total > 0 || complete {
			delete(unknown, key)
		} else {
			unknown[key] = true
		}
		coverage[key] = slices.Clone(written)
		if tick == nil {
			return
		}
		var done, sum int64
		ranges := make(map[string][]Range, len(coverage))
		for k, rows := range coverage {
			done += rangeBytes(rows)
			ranges[k] = slices.Clone(rows)
		}
		if len(unknown) == 0 {
			for _, t := range totals {
				sum += t
			}
		}
		tick(done, sum, key, ranges)
	}
	for _, src := range selected {
		key := src.Dest
		var size int64
		_, err := d.trySources(ctx, src, failures[key], func(c Source) error {
			n, err := d.transfer(ctx, c, func(written []Range, total int64) { report(key, written, total, false) })
			size = n
			return err
		})
		if err != nil {
			return err
		}
		report(key, []Range{{0, size}}, size, true)
	}
	return nil
}

// Durable byte count of the transfer in flight, read by the retry loop to renew its budget
type transferState struct{ durable atomic.Int64 }

// Downloads src under the lock of its partial, then verifies and publishes it
func (d *downloader) transfer(ctx context.Context, src Source, tick func([]Range, int64)) (int64, error) {
	l, err := fileutil.OpenLock(ctx, d.lockPath(src.URL), fileutil.LockPerm(0o600))
	if err != nil {
		return 0, err
	}
	defer l.Release()
	if ok, err := existingDestOK(src); err != nil {
		return 0, err
	} else if ok {
		fi, err := os.Stat(src.Dest)
		if err != nil {
			return 0, err
		}
		return fi.Size(), nil
	}
	remote, err := d.probe(ctx, src)
	if err != nil {
		return 0, err
	}
	var covered []Range
	if remote.ranged {
		covered = d.loadSidecar(src, remote)
	}
	st := &transferState{}
	st.durable.Store(rangeBytes(covered))
	tick(covered, remote.total)
	resumable := remote.total > 0 && remote.ranged && (src.SHA256 != "" || remote.etag != "")
	report := func(written []Range) { tick(written, remote.total) }
	connections := d.connections
	var size int64
	fetch := func() error {
		var err error
		if resumable {
			size, err = d.fetchRanged(ctx, src, remote, &connections, st, report)
		} else {
			size, err = d.fetchSingle(ctx, src, remote, report)
		}
		return err
	}
	var progress func() int64
	if resumable {
		progress = st.durable.Load
	}
	if err := retryNetwork(ctx, fetch, progress, len(src.Fallbacks) > 0); err != nil {
		switch {
		case ctx.Err() != nil:
			return 0, ctx.Err()
		case networkFailure(err):
			return 0, transportFailure(src.URL, err, d.mirrorHost)
		}
		return 0, err
	}
	return size, d.finalize(ctx, src)
}

// Fills the gaps of the partial over parallel range requests until every byte is durable
func (d *downloader) fetchRanged(ctx context.Context, src Source, remote remoteFile, connections *int, st *transferState, report func([]Range)) (int64, error) {
	part, side := d.partPaths(src.URL)
	covered := d.loadSidecar(src, remote)
	flags := os.O_RDWR | os.O_CREATE
	if len(covered) == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o666)
	if err != nil {
		return 0, err
	}
	if err := errors.Join(f.Truncate(remote.total), f.Close()); err != nil {
		return 0, err
	}
	for {
		gaps := int64(len(missing(remote.total, covered)))
		if len(covered) == 0 {
			gaps = (remote.total + downloadChunk - 1) / downloadChunk
		}
		workers := int(max(1, min(int64(*connections), gaps)))
		var errs []error
		covered, errs = d.rangedAttempt(ctx, src, remote, covered, workers, st, report)
		for _, e := range errs {
			var re *rangeError
			if errors.As(e, &re) {
				_ = os.Remove(part)
				_ = os.Remove(side)
				return 0, e
			}
		}
		if err := d.writeSidecar(src, remote, covered); err != nil {
			return 0, err
		}
		written := rangeBytes(covered)
		st.durable.Store(written)
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if len(errs) > 0 {
			if *connections > 1 && refusedParallel(errs) {
				slog.Warn(fmt.Sprintf("parallel range fetch refused for %s; retrying with one connection", src.URL))
				*connections = 1
				continue
			}
			for _, e := range errs {
				if !isTransient(e) {
					return 0, e
				}
			}
			return 0, errs[0]
		}
		if written != remote.total {
			return 0, errIncomplete
		}
		return written, nil
	}
}

// Reports whether every worker was refused with a forbidden or not found status
func refusedParallel(errs []error) bool {
	for _, e := range errs {
		var st *statusError
		if !errors.As(e, &st) || (st.code != http.StatusForbidden && st.code != http.StatusNotFound) {
			return false
		}
	}
	return true
}

// One pass of range workers over the open gaps, checkpointing and reporting what they made durable
func (d *downloader) rangedAttempt(ctx context.Context, src Source, remote remoteFile, covered []Range, workers int, st *transferState, report func([]Range)) ([]Range, []error) {
	ranges := missing(remote.total, covered)
	if len(covered) == 0 {
		count := max(1, min(int64(workers), (remote.total+downloadChunk-1)/downloadChunk))
		ranges = ranges[:0]
		for i := int64(0); i < count; i++ {
			ranges = append(ranges, Range{i * remote.total / count, (i + 1) * remote.total / count})
		}
	}
	var mu sync.Mutex
	var errs []error
	current := slices.Clone(covered)
	work, stop := context.WithCancel(ctx)
	defer stop()
	updated := make(chan struct{}, 1)
	record := func(start, position int64) {
		mu.Lock()
		current = coalesce(append(current, Range{start, position}))
		mu.Unlock()
		select {
		case updated <- struct{}{}:
		default:
		}
	}
	slots := make(chan struct{}, workers)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, r := range ranges {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if err := d.readRange(work, src, remote, r, record); err != nil && work.Err() == nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				stop()
			}
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	reported, checkpointed := slices.Clone(covered), slices.Clone(covered)
	timer := time.NewTimer(checkpointInterval)
	defer timer.Stop()
	for finished := false; !finished; {
		checkpoint := false
		select {
		case <-done:
			finished = true
		case <-updated:
		case <-timer.C:
			checkpoint = true
			timer.Reset(checkpointInterval)
		}
		mu.Lock()
		snapshot := slices.Clone(current)
		mu.Unlock()
		if checkpoint && !slices.Equal(snapshot, checkpointed) {
			if err := d.writeSidecar(src, remote, snapshot); err != nil {
				slog.Warn(fmt.Sprintf("could not save download progress for %s; continuing: %v", filepath.Base(src.Dest), err))
			} else {
				st.durable.Store(rangeBytes(snapshot))
				checkpointed = snapshot
			}
		}
		if !slices.Equal(snapshot, reported) {
			report(snapshot)
			reported = snapshot
		}
	}
	return current, errs
}

// Streams the bytes of one range into their place in the partial, recording each chunk once written
func (d *downloader) readRange(ctx context.Context, src Source, remote remoteFile, r Range, record func(start, position int64)) error {
	if ctx.Err() != nil {
		return nil
	}
	headers := map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", r.Start, r.End-1)}
	if remote.etag != "" {
		headers["If-Range"] = remote.etag
	}
	resp, err := d.get(ctx, src.URL, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := validateRange(resp, r.Start, r.End, remote.total); err != nil {
		return err
	}
	if remote.etag != "" && strongETag(resp) != remote.etag {
		return &rangeError{"remote representation changed during download"}
	}
	part, _ := d.partPaths(src.URL)
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, downloadChunk)
	for position := r.Start; position < r.End; {
		if ctx.Err() != nil {
			return nil
		}
		n, readErr := io.ReadFull(resp.Body, buf[:min(downloadChunk, r.End-position)])
		if n > 0 {
			if _, err := f.WriteAt(buf[:n], position); err != nil {
				return err
			}
			position += int64(n)
			if position == r.End {
				if extra, _ := resp.Body.Read(make([]byte, 1)); extra > 0 {
					return &rangeError{"range body exceeds its declared bounds"}
				}
			}
			record(r.Start, position)
		}
		switch {
		case readErr == nil:
		case errors.Is(readErr, io.EOF), errors.Is(readErr, io.ErrUnexpectedEOF):
			return errIncomplete
		default:
			return readErr
		}
	}
	return nil
}

// Streams the whole file over one connection when the server gives no way to resume safely
func (d *downloader) fetchSingle(ctx context.Context, src Source, remote remoteFile, report func([]Range)) (int64, error) {
	part, _ := d.partPaths(src.URL)
	resp, err := d.get(ctx, src.URL, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode != http.StatusOK:
		return 0, fmt.Errorf("unexpected download status: %d", resp.StatusCode)
	case remote.etag != "" && strongETag(resp) != remote.etag:
		return 0, &rangeError{"remote representation changed during download"}
	case encoded(resp):
		return 0, errors.New("encoded response cannot be used as an identity download")
	}
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return 0, err
	}
	declared, limit := max(resp.ContentLength, 0), limitFor(src)
	var position int64
	var covered []Range
	buf := make([]byte, downloadChunk)
	var failure error
	for failure == nil {
		if failure = ctx.Err(); failure != nil {
			break
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, failure = f.Write(buf[:n]); failure != nil {
				break
			}
			position += int64(n)
			if position > limit {
				failure = &SizeError{URL: src.URL, Size: position, Limit: limit}
				break
			}
			covered = []Range{{0, position}}
			report(covered)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		failure = readErr
	}
	saveErr := errors.Join(f.Close(), d.writeSidecar(src, remote, covered))
	if failure != nil {
		return 0, failure
	}
	if saveErr != nil {
		return 0, saveErr
	}
	want := declared
	if want == 0 {
		want = remote.total
	}
	if position < want {
		return 0, errIncomplete
	}
	if (declared > 0 && position != declared) || (remote.total > 0 && position != remote.total) {
		return 0, fmt.Errorf("download incomplete (%d bytes, expected %d)", position, want)
	}
	return position, nil
}

// Verifies the finished partial against its pin, copies it beside the destination and renames it into place
func (d *downloader) finalize(ctx context.Context, src Source) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	part, side := d.partPaths(src.URL)
	if src.SHA256 != "" {
		got, err := sha256File(part)
		if err != nil {
			return err
		}
		if got != src.SHA256 {
			_ = os.Remove(part)
			_ = os.Remove(side)
			return &HashError{URL: src.URL, Want: src.SHA256, Got: got}
		}
	}
	if err := os.MkdirAll(filepath.Dir(src.Dest), 0o777); err != nil {
		return err
	}
	staged, err := os.CreateTemp(filepath.Dir(src.Dest), "."+filepath.Base(src.Dest)+"-*.download")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	in, err := os.Open(part)
	if err != nil {
		return errors.Join(err, staged.Close())
	}
	copied, err := io.Copy(staged, in)
	if err := errors.Join(err, in.Close(), staged.Sync(), staged.Close()); err != nil {
		return err
	}
	if fi, err := os.Stat(part); err != nil || fi.Size() != copied {
		return errors.New("destination copy did not preserve the complete download")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replaceWhenReleased(ctx, staged.Name(), src.Dest); err != nil {
		return err
	}
	_ = os.Remove(part)
	_ = os.Remove(side)
	return nil
}

// Renames a finished download into place, waiting out a scanner that still holds the file open
func replaceWhenReleased(ctx context.Context, tmp, dest string) error {
	deadline := time.Now().Add(releaseWait)
	delay := 100 * time.Millisecond
	for {
		err := os.Rename(tmp, dest)
		if err == nil || !errors.Is(err, fs.ErrPermission) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("The download finished, but another program (usually an antivirus scan) kept %s open and it could not be renamed into place. Please try again.: %w",
				filepath.Base(tmp), err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 2*time.Second)
	}
}

// How a caller driven download runs
type DownloadOptions struct {
	// Directory partial bytes and their state are kept in, the shared partials directory of the install when empty
	Partials string
	// Parallel range requests per file, eight when zero
	Connections int
	// Receives bytes done and expected over the whole plan, the destination reporting and the ranges on disk by destination
	Progress func(done, total int64, active string, ranges map[string][]Range)
}

// Fetches each source into its destination, resumable, hashed when pinned, every source bounded by Size or Limit
func (s *Store) Download(ctx context.Context, sources []Source, o DownloadOptions) error {
	for _, src := range sources {
		if src.Size <= 0 && src.Limit <= 0 {
			return fmt.Errorf("download of %s has no size bound", src.URL)
		}
	}
	partials := o.Partials
	if partials == "" {
		shared, err := s.partialsRoot(ctx)
		if err != nil {
			return err
		}
		partials = shared
	}
	return s.downloader(partials, o.Connections).run(ctx, sources, downloadTick(o.Progress))
}

// Deletes partials no process owns once neither file was written within the grace period
func collectPartials(dir string, grace time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keys := map[string]bool{}
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); ext == ".part" || ext == ".ranges" {
			keys[strings.TrimSuffix(e.Name(), ext)] = true
		}
	}
	now := time.Now()
	for key := range keys {
		l, err := fileutil.TryOpenLock(filepath.Join(dir, ".locks", key), fileutil.LockPerm(0o600))
		if err != nil {
			continue
		}
		pair := []string{filepath.Join(dir, key+".part"), filepath.Join(dir, key+".ranges")}
		fresh := false
		for _, p := range pair {
			if fi, err := os.Stat(p); err == nil && grace > 0 && now.Sub(fi.ModTime()) < grace {
				fresh = true
			}
		}
		if !fresh {
			for _, p := range pair {
				_ = os.Remove(p)
			}
		}
		_ = l.Release()
	}
}
