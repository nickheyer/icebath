package packages

import (
	"cmp"
	"context"
	"crypto/sha256"
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
	"time"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Ranges one download fetches at once
const connections = 8

const (
	chunk           = 1 << 20
	probeTimeout    = 60 * time.Second
	transferTimeout = 120 * time.Second
	// Longest a finished download waits for another program to let go of its place
	releaseWait = 60 * time.Second
	// How often a ranged transfer saves what it has
	checkpoint = 5 * time.Second
	// Age past which gc removes a partial nobody works on
	partialGrace = 6 * time.Hour
)

var (
	fullSHA256   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	contentRange = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+)$`)
)

// What kind of failure a download had
type DownloadKind int

const (
	Failed DownloadKind = iota
	// The server changed the bytes or the range under the download
	RangeChanged
	// The bytes are not the pinned ones
	HashMismatch
	// Stopped by its caller with its partial kept for the next run
	Paused
	// The network or server refused once every try was spent
	Unreachable
)

type DownloadError struct {
	Kind    DownloadKind
	Message string
	URL     string
	Status  int
	// A pinned mirror may be tried after it
	Fallback bool
	Err      error
}

func (e *DownloadError) Error() string { return e.Message }

func (e *DownloadError) Unwrap() error { return e.Err }

func downloadError(kind DownloadKind, format string, args ...any) *DownloadError {
	return &DownloadError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Whether err is a download failure of the kind
func IsDownload(err error, kind DownloadKind) bool {
	de, ok := errors.AsType[*DownloadError](err)
	return ok && de.Kind == kind
}

// Network failure once every try was spent, with whether a mirror may take over
func unreachable(rawURL string, cause error) *DownloadError {
	status := 0
	if he, ok := errors.AsType[*HTTPError](cause); ok {
		status = he.Code
	}
	_, unresolved := errors.AsType[*net.DNSError](cause)
	reason := cause.Error()
	if he, ok := errors.AsType[*HTTPError](cause); ok {
		reason = he.Error()
	} else if ue, ok := errors.AsType[*url.Error](cause); ok {
		reason = ue.Err.Error()
		if ue.Timeout() {
			reason = "the connection timed out"
		}
	}
	refused := edgeBlock(cause)
	if refused == "" && (status == 401 || status == 403) {
		refused = "the host refused access"
	}
	if refused != "" {
		reason += "; " + refused
	}
	return &DownloadError{Kind: Unreachable, Message: fmt.Sprintf("download failed from %s: %s", rawURL, reason), URL: rawURL, Status: status,
		Fallback: status == 401 || status == 403 || status == 404 || status == 410 || status >= 500 || unresolved || transient(cause), Err: cause}
}

// Half open byte interval
type Range struct{ Start, End int64 }

// Runs one destination holds, keyed by its address
type Coverage struct {
	Key    string
	Ranges []Range
}

// Bytes done of every destination and their total, the last coverage being the destination that moved; an error stops the download
type Progress func(done, total int64, coverage []Coverage) error

// One file of a download: where it comes from, where it goes, its pinned digest and the pinned mirrors that serve the same bytes
type Source struct {
	URL       string
	Dest      string
	SHA256    string
	Fallbacks []string
}

// Resumable download of a set of files, ranges of each fetched at once and every file checked before it is published
type Download struct {
	Sources []Source
	// Starts afresh rather than from the partial an earlier run kept
	NoResume bool
	// Ranges at once, eight when zero
	Connections int
	// Folder of the partials, the package manager's own when empty
	Partials string
	// Bytes saved for good of the transfer running, renewing its tries
	durable atomic.Int64
}

// Download through the package manager's partials
func (h *Packages) Download(sources []Source) *Download {
	return &Download{Sources: sources, Partials: h.partials()}
}

// What a probe learned of the remote file
type remote struct {
	total  int64
	ranged bool
	etag   string
}

// Fetches every source, the context's end stopping it as paused with its partials kept; the published destinations in source order
func (d *Download) Run(ctx context.Context, progress Progress) ([]string, error) {
	conns := cmp.Or(d.Connections, connections)
	if err := paused(ctx); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d.Partials, 0o777); err != nil {
		return nil, err
	}
	for _, s := range d.Sources {
		if len(s.Fallbacks) > 0 && !fullSHA256.MatchString(s.SHA256) {
			return nil, errors.New("mirror fallback requires a full lowercase SHA256")
		}
		for _, u := range append([]string{s.URL}, s.Fallbacks...) {
			if !strings.HasPrefix(u, "https://") && !loopback(u) {
				return nil, fmt.Errorf("refusing non-https url: %s", u)
			}
		}
	}
	totals, unknown := map[string]int64{}, map[string]bool{}
	var coverage []Coverage
	cover := func(key string, rows []Range) {
		coverage = slices.DeleteFunc(coverage, func(c Coverage) bool { return c.Key == key })
		coverage = append(coverage, Coverage{Key: key, Ranges: slices.Clone(rows)})
	}
	failures := map[string]*[]*DownloadError{}
	var selected []Source
	for _, s := range d.Sources {
		if err := paused(ctx); err != nil {
			return nil, err
		}
		failures[s.Dest] = &[]*DownloadError{}
		done, err := present(s)
		if err != nil {
			return nil, err
		}
		var info remote
		var covered []Range
		if done {
			st, _ := os.Stat(s.Dest)
			info.total = st.Size()
			covered = []Range{{0, info.total}}
		} else {
			s, err = d.trying(ctx, s, func(c Source) error {
				var err error
				info, err = d.probe(ctx, c.URL)
				return err
			}, failures[s.Dest])
			if err != nil {
				return nil, err
			}
			release, owned, err := d.claim(ctx, s.URL, false)
			if err != nil {
				return nil, err
			}
			if owned && info.ranged {
				covered = d.saved(s, info)
			}
			release()
			if info.total == 0 {
				unknown[s.Dest] = true
			}
		}
		selected = append(selected, s)
		totals[s.Dest] = info.total
		cover(s.Dest, covered)
	}
	report := func(key string, written []Range, total int64, complete bool) error {
		totals[key] = total
		if total != 0 || complete {
			delete(unknown, key)
		} else {
			unknown[key] = true
		}
		cover(key, written)
		if progress == nil {
			return nil
		}
		var done, sum int64
		for _, c := range coverage {
			done += size(c.Ranges)
		}
		if len(unknown) == 0 {
			for _, t := range totals {
				sum += t
			}
		}
		snapshot := make([]Coverage, len(coverage))
		for i, c := range coverage {
			snapshot[i] = Coverage{Key: c.Key, Ranges: slices.Clone(c.Ranges)}
		}
		return progress(done, sum, snapshot)
	}
	for _, s := range selected {
		key := s.Dest
		var n int64
		if _, err := d.trying(ctx, s, func(c Source) error {
			var err error
			n, err = d.transfer(ctx, c, conns, func(written []Range, total int64) error { return report(key, written, total, false) })
			return err
		}, failures[key]); err != nil {
			return nil, err
		}
		if err := report(key, []Range{{0, n}}, n, true); err != nil {
			return nil, err
		}
	}
	out := make([]string, len(d.Sources))
	for i, s := range d.Sources {
		out[i] = s.Dest
	}
	return out, nil
}

// Paused failure once the caller stopped the download
func paused(ctx context.Context) error {
	if ctx.Err() != nil {
		return &DownloadError{Kind: Paused, Message: "download paused", Err: ctx.Err()}
	}
	return nil
}

// Runs an operation against the source, then each pinned mirror while the failures let one take over
func (d *Download) trying(ctx context.Context, s Source, operation func(Source) error, failures *[]*DownloadError) (Source, error) {
	var urls []string
	for _, u := range append([]string{s.URL}, s.Fallbacks...) {
		if !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
	}
	for i, u := range urls {
		if err := paused(ctx); err != nil {
			return Source{}, err
		}
		candidate := Source{URL: u, Dest: s.Dest, SHA256: s.SHA256, Fallbacks: slices.Clone(urls[i+1:])}
		err := operation(candidate)
		if err == nil {
			return candidate, nil
		}
		de, ok := errors.AsType[*DownloadError](err)
		if !ok || de.Kind != Unreachable {
			return Source{}, err
		}
		*failures = append(*failures, de)
		if !de.Fallback || i == len(urls)-1 {
			if len(*failures) == 1 {
				return Source{}, de
			}
			messages := make([]string, len(*failures))
			for j, f := range *failures {
				messages[j] = f.Error()
			}
			return Source{}, &DownloadError{Kind: Failed, Message: strings.Join(messages, "\n"), Err: de}
		}
		slog.Debug(fmt.Sprintf("%s; trying pinned mirror %s", de, urls[i+1]))
	}
	return Source{}, downloadError(Failed, "no download url for %s", s.Dest)
}

// Whether the destination already holds the wanted bytes, a pinned one hashed again
func present(s Source) (bool, error) {
	st, err := os.Stat(s.Dest)
	if err != nil {
		return false, nil
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("download destination is not a file: %s", s.Dest)
	}
	if s.SHA256 == "" {
		return true, nil
	}
	actual, err := fsx.Digest(s.Dest)
	return err == nil && actual == s.SHA256, nil
}

// Learns the length, range support and strong validator of a file with a one byte range request
func (d *Download) probe(ctx context.Context, rawURL string) (remote, error) {
	info, err := retry(ctx, func() (remote, error) {
		if err := paused(ctx); err != nil {
			return remote{}, err
		}
		resp, err := open(ctx, rawURL, map[string]string{"Range": "bytes=0-0"}, probeTimeout)
		if err != nil {
			return remote{}, err
		}
		defer resp.Body.Close()
		etag := strongETag(resp.Header.Get("ETag"))
		if resp.StatusCode == 206 {
			total, err := validRange(resp, 0, 1, 0, false)
			if err != nil {
				return remote{}, err
			}
			body, err := readUpTo(resp.Body, 2)
			if err != nil {
				return remote{}, err
			}
			switch len(body) {
			case 0:
				return remote{}, &shortRead{expected: 1}
			case 1:
				return remote{total: total, ranged: true, etag: etag}, nil
			}
			return remote{}, downloadError(RangeChanged, "range probe returned the wrong byte count")
		}
		if resp.StatusCode != 200 {
			return remote{}, downloadError(Failed, "unexpected download probe status: %d", resp.StatusCode)
		}
		total, _ := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64)
		return remote{total: total, etag: etag}, nil
	}, retrying{})
	if err == nil {
		return info, nil
	}
	if _, ok := errors.AsType[*DownloadError](err); ok {
		return remote{}, err
	}
	return remote{}, unreachable(rawURL, err)
}

// A quoted strong validator, empty for a weak or missing one
func strongETag(value string) string {
	if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		return value
	}
	return ""
}

// Size of the file a range response covers, refusing one that is not exactly the range asked for
func validRange(resp *http.Response, start, end, total int64, haveTotal bool) (int64, error) {
	m := contentRange.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if resp.StatusCode != 206 || m == nil {
		return 0, downloadError(RangeChanged, "server did not honor the requested byte range")
	}
	first, _ := strconv.ParseInt(m[1], 10, 64)
	last, _ := strconv.ParseInt(m[2], 10, 64)
	length, _ := strconv.ParseInt(m[3], 10, 64)
	if first != start || last != end-1 || length < end || (haveTotal && length != total) {
		return 0, downloadError(RangeChanged, "server returned a different byte range or representation size")
	}
	if values := resp.Header.Values("Content-Length"); len(values) > 0 {
		if n, err := strconv.ParseInt(values[0], 10, 64); err != nil || n != end-start {
			return 0, downloadError(RangeChanged, "range Content-Length does not match its bounds")
		}
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return 0, downloadError(RangeChanged, "encoded response cannot be written into an identity byte range")
	}
	return length, nil
}

// Reads up to n bytes, fewer meaning the body ended
func readUpTo(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return buf[:got], err
}

// Partial and sidecar names of an address
func (d *Download) files(rawURL string) (string, string) {
	sum := sha256.Sum256([]byte(rawURL))
	key := filepath.Join(d.Partials, hex.EncodeToString(sum[:]))
	return key + ".part", key + ".ranges"
}

// Takes the lock that makes one process the owner of an address's partial, waiting for it when asked to
func (d *Download) claim(ctx context.Context, rawURL string, wait bool) (func(), bool, error) {
	sum := sha256.Sum256([]byte(rawURL))
	path := filepath.Join(d.Partials, ".locks", hex.EncodeToString(sum[:]))
	if !wait {
		if release, ok, err := fsx.TryLock(path); err != nil || ok {
			return release, ok, err
		}
		return func() {}, false, nil
	}
	release, err := fsx.Lock(ctx, path, -1)
	if ctx.Err() != nil {
		return func() {}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return release, true, nil
}

// Sidecar of a partial: the length, validator and pin it was fetched under, and the runs it holds
type sidecar struct {
	Total  int64      `json:"total"`
	ETag   string     `json:"etag"`
	SHA256 string     `json:"sha256"`
	Ranges [][2]int64 `json:"ranges"`
}

// Runs of a source's partial still bound to the same length, validator and pin, none otherwise
func (d *Download) saved(s Source, info remote) []Range {
	if d.NoResume || (s.SHA256 == "" && info.etag == "") {
		return nil
	}
	part, side := d.files(s.URL)
	data, err := os.ReadFile(side)
	if err != nil {
		return nil
	}
	var car sidecar
	if json.Unmarshal(data, &car) != nil || car.Total != info.total || car.ETag != info.etag || car.SHA256 != s.SHA256 {
		return nil
	}
	st, err := os.Stat(part)
	if err != nil {
		return nil
	}
	limit := min(st.Size(), info.total)
	var ranges []Range
	for _, row := range car.Ranges {
		if !(0 <= row[0] && row[0] < row[1] && row[1] <= limit) {
			return nil
		}
		ranges = append(ranges, Range{row[0], row[1]})
	}
	return coalesce(ranges)
}

// Syncs the partial then publishes the runs it holds beside it
func writeSidecar(part, side string, covered []Range, info remote, sha string) error {
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
	car := sidecar{Total: info.total, ETag: info.etag, SHA256: sha, Ranges: [][2]int64{}}
	for _, r := range covered {
		car.Ranges = append(car.Ranges, [2]int64{r.Start, r.End})
	}
	body, err := json.Marshal(car)
	if err != nil {
		return err
	}
	return fsx.Write(side, body, 0o600)
}

// Transfers one source as the owner of its partial, then publishes it once its bytes check out
func (d *Download) transfer(ctx context.Context, s Source, conns int, tick func([]Range, int64) error) (int64, error) {
	release, ok, err := d.claim(ctx, s.URL, true)
	if err != nil {
		return 0, err
	}
	defer release()
	if !ok {
		return 0, &DownloadError{Kind: Paused, Message: s.URL, Err: ctx.Err()}
	}
	if done, err := present(s); err != nil || done {
		if err != nil {
			return 0, err
		}
		st, err := os.Stat(s.Dest)
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	}
	info, err := d.probe(ctx, s.URL)
	if err != nil {
		return 0, err
	}
	var covered []Range
	if info.ranged {
		covered = d.saved(s, info)
	}
	d.durable.Store(size(covered))
	if err := tick(covered, info.total); err != nil {
		return 0, err
	}
	resumable := info.total != 0 && info.ranged && (s.SHA256 != "" || info.etag != "")
	report := func(written []Range) error { return tick(written, info.total) }
	opts := retrying{mirror: len(s.Fallbacks) > 0}
	if !d.NoResume && resumable {
		opts.progress = d.durable.Load
	}
	n, err := retry(ctx, func() (int64, error) {
		if err := paused(ctx); err != nil {
			return 0, err
		}
		if resumable {
			return d.ranged(ctx, s, info, conns, report)
		}
		return d.single(ctx, s, info, report)
	}, opts)
	if err != nil {
		if _, ok := errors.AsType[*DownloadError](err); !ok {
			if _, isURL := errors.AsType[*url.Error](err); isURL || transient(err) {
				return 0, unreachable(s.URL, err)
			}
			if _, isHTTP := errors.AsType[*HTTPError](err); isHTTP {
				return 0, unreachable(s.URL, err)
			}
		}
		return 0, err
	}
	part, side := d.files(s.URL)
	return n, d.finish(ctx, s, part, side)
}

// Fetches the missing runs of a resumable source, several at once, until its partial is whole
func (d *Download) ranged(ctx context.Context, s Source, info remote, conns int, tick func([]Range) error) (int64, error) {
	part, side := d.files(s.URL)
	covered := d.saved(s, info)
	flags := os.O_RDWR | os.O_CREATE
	if len(covered) == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o666)
	if err != nil {
		return 0, err
	}
	if err := f.Truncate(info.total); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	for {
		gaps := (info.total + chunk - 1) / chunk
		if len(covered) > 0 {
			gaps = int64(len(missing(info.total, covered)))
		}
		var errs []error
		covered, errs = d.pass(ctx, s, info, part, covered, int(max(1, min(int64(conns), gaps))), tick)
		for _, e := range errs {
			if IsDownload(e, RangeChanged) {
				os.Remove(part)
				os.Remove(side)
				return 0, e
			}
		}
		if err := writeSidecar(part, side, covered, info, s.SHA256); err != nil {
			return 0, err
		}
		d.durable.Store(size(covered))
		if err := paused(ctx); err != nil {
			return 0, err
		}
		if len(errs) > 0 {
			if conns > 1 && refusedAll(errs) {
				slog.Warn(fmt.Sprintf("parallel range fetch refused for %s; retrying with one connection", s.URL))
				conns = 1
				continue
			}
			for _, e := range errs {
				if !transient(e) {
					return 0, e
				}
			}
			return 0, errs[0]
		}
		if written := size(covered); written != info.total {
			return 0, &shortRead{expected: info.total - written}
		}
		return info.total, nil
	}
}

// Whether every range failure was a 403 or 404, a server refusing parallel ranges
func refusedAll(errs []error) bool {
	for _, e := range errs {
		if he, ok := errors.AsType[*HTTPError](e); !ok || (he.Code != 403 && he.Code != 404) {
			return false
		}
	}
	return true
}

// One parallel pass over the missing runs, saving and reporting what is held from one place
func (d *Download) pass(ctx context.Context, s Source, info remote, part string, covered []Range, conns int, tick func([]Range) error) ([]Range, []error) {
	ranges := missing(info.total, covered)
	if len(covered) == 0 {
		count := max(1, min(int64(conns), (info.total+chunk-1)/chunk))
		ranges = nil
		for i := range count {
			ranges = append(ranges, Range{i * info.total / count, (i + 1) * info.total / count})
		}
	}
	stopCtx, stop := context.WithCancel(ctx)
	defer stop()
	var mu sync.Mutex
	var errs []error
	pending := len(ranges)
	covered = slices.Clone(covered)
	updated := make(chan struct{}, 1)
	signal := func() {
		select {
		case updated <- struct{}{}:
		default:
		}
	}
	record := func(start, position int64) {
		mu.Lock()
		covered = coalesce(append(slices.Clone(covered), Range{start, position}))
		mu.Unlock()
		signal()
	}
	slots := make(chan struct{}, conns)
	var wg sync.WaitGroup
	for _, r := range ranges {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			err := d.read(stopCtx, s, info, part, r.Start, r.End, record)
			mu.Lock()
			if err != nil && stopCtx.Err() == nil {
				errs = append(errs, err)
			}
			pending--
			mu.Unlock()
			if err != nil {
				stop()
			}
			signal()
		})
	}
	reported, saved := slices.Clone(covered), slices.Clone(covered)
	next := time.Now().Add(checkpoint)
	failed := false
	for finished := len(ranges) == 0; !finished; {
		timer := time.NewTimer(max(0, time.Until(next)))
		select {
		case <-updated:
		case <-timer.C:
		}
		timer.Stop()
		mu.Lock()
		snapshot := slices.Clone(covered)
		finished = pending == 0
		mu.Unlock()
		if !time.Now().Before(next) {
			if !slices.Equal(snapshot, saved) {
				_, side := d.files(s.URL)
				if err := writeSidecar(part, side, snapshot, info, s.SHA256); err != nil {
					slog.Warn(fmt.Sprintf("could not save download progress for %s; continuing: %s", filepath.Base(s.Dest), err))
				} else {
					d.durable.Store(size(snapshot))
					saved = snapshot
				}
			}
			next = time.Now().Add(checkpoint)
		}
		if !failed && !slices.Equal(snapshot, reported) {
			if err := tick(snapshot); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				failed = true
				stop()
			}
			reported = snapshot
		}
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	return covered, errs
}

// Streams one byte range into the partial, recording what is written after every chunk
func (d *Download) read(ctx context.Context, s Source, info remote, part string, start, end int64, record func(int64, int64)) error {
	if ctx.Err() != nil {
		return nil
	}
	headers := map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end-1)}
	if info.etag != "" {
		headers["If-Range"] = info.etag
	}
	resp, err := open(ctx, s.URL, headers, transferTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	if _, err := validRange(resp, start, end, info.total, true); err != nil {
		return err
	}
	if info.etag != "" && strongETag(resp.Header.Get("ETag")) != info.etag {
		return downloadError(RangeChanged, "remote representation changed during download")
	}
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	for position := start; position < end; {
		if ctx.Err() != nil {
			return nil
		}
		data, err := readUpTo(resp.Body, int(min(chunk, end-position)))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if len(data) == 0 {
			return &shortRead{expected: end - position}
		}
		if _, err := f.WriteAt(data, position); err != nil {
			return err
		}
		position += int64(len(data))
		if position == end {
			if extra, err := readUpTo(resp.Body, 1); err != nil {
				return err
			} else if len(extra) > 0 {
				return downloadError(RangeChanged, "range body exceeds its declared bounds")
			}
		}
		record(start, position)
	}
	return nil
}

// Streams a whole body when no strong validator lets earlier bytes be reused
func (d *Download) single(ctx context.Context, s Source, info remote, tick func([]Range) error) (written int64, err error) {
	part, side := d.files(s.URL)
	var covered []Range
	defer func() {
		if _, statErr := os.Stat(part); statErr == nil || err == nil {
			if serr := writeSidecar(part, side, covered, info, s.SHA256); serr != nil && err == nil {
				err = serr
			}
		}
	}()
	resp, err := open(ctx, s.URL, nil, transferTimeout)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if info.etag != "" && strongETag(resp.Header.Get("ETag")) != info.etag {
		return 0, downloadError(RangeChanged, "remote representation changed during download")
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return 0, downloadError(Failed, "encoded response cannot be used as an identity download")
	}
	declared, _ := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64)
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var position int64
	for {
		if err := paused(ctx); err != nil {
			return 0, err
		}
		data, err := readUpTo(resp.Body, chunk)
		if err != nil {
			if perr := paused(ctx); perr != nil {
				return 0, perr
			}
			return 0, err
		}
		if len(data) == 0 {
			break
		}
		if _, err := f.Write(data); err != nil {
			return 0, err
		}
		position += int64(len(data))
		covered = []Range{{0, position}}
		if err := tick(slices.Clone(covered)); err != nil {
			return 0, err
		}
	}
	expected := cmp.Or(declared, info.total)
	if position < expected {
		return 0, &shortRead{expected: expected - position}
	}
	if (declared != 0 && position != declared) || (info.total != 0 && position != info.total) {
		return 0, downloadError(Failed, "download incomplete (%d bytes, expected %d)", position, expected)
	}
	return position, nil
}

// Checks the pin, copies the partial beside the destination durably and swaps it into place
func (d *Download) finish(ctx context.Context, s Source, part, side string) error {
	if err := paused(ctx); err != nil {
		return err
	}
	if s.SHA256 != "" {
		actual, err := fsx.Digest(part)
		if err != nil {
			return err
		}
		if actual != s.SHA256 {
			if err := os.Remove(part); err == nil || os.IsNotExist(err) {
				os.Remove(side)
			}
			return downloadError(HashMismatch, "sha256 mismatch for %s: pinned %s, got %s", s.URL, s.SHA256, actual)
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.Dest), 0o777); err != nil {
		return err
	}
	staged, err := os.CreateTemp(filepath.Dir(s.Dest), "."+filepath.Base(s.Dest)+"-*.download")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	in, err := os.Open(part)
	if err != nil {
		staged.Close()
		return err
	}
	_, err = io.CopyBuffer(staged, in, make([]byte, chunk))
	in.Close()
	if err == nil {
		err = staged.Sync()
	}
	if cerr := staged.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := paused(ctx); err != nil {
		return err
	}
	if err := placeWhenReleased(staged.Name(), s.Dest); err != nil {
		return err
	}
	if err := os.Remove(part); err == nil || os.IsNotExist(err) {
		os.Remove(side)
	}
	return nil
}

// Renames a finished download into place, waiting out another program holding it for up to a minute
func placeWhenReleased(tmp, dest string) error {
	deadline := time.Now().Add(releaseWait)
	delay := 100 * time.Millisecond
	for {
		err := os.Rename(tmp, dest)
		if err == nil || !fsx.Held(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("The download finished, but another program (usually an antivirus scan) kept %s open and it could not be renamed into place. Please try again: %w", filepath.Base(tmp), err)
		}
		time.Sleep(delay)
		delay = min(2*delay, 2*time.Second)
	}
}

// Removes partials nobody owns that nothing touched for the grace period
func collectPartials(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	keys := map[string]bool{}
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); ext == ".part" || ext == ".ranges" {
			keys[strings.TrimSuffix(e.Name(), ext)] = true
		}
	}
	for key := range keys {
		release, ok, err := fsx.TryLock(filepath.Join(root, ".locks", key))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		pair := []string{filepath.Join(root, key+".part"), filepath.Join(root, key+".ranges")}
		fresh := false
		for _, path := range pair {
			if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < partialGrace {
				fresh = true
			}
		}
		if !fresh {
			for _, path := range pair {
				os.Remove(path)
			}
		}
		release()
	}
	return nil
}

// Hex sha256 of the file at an address, the way a pin is made
func hashURL(ctx context.Context, rawURL string) (string, error) {
	return retry(ctx, func() (string, error) {
		resp, err := open(ctx, rawURL, nil, 600*time.Second)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		h := sha256.New()
		n, err := io.CopyBuffer(h, resp.Body, make([]byte, chunk))
		if err != nil {
			return "", err
		}
		if declared, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); n < declared {
			return "", &shortRead{expected: declared - n}
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}, retrying{})
}

// Merged sorted runs of the ranges
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
	slices.SortFunc(runs, func(a, b Range) int { return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End)) })
	out := []Range{runs[0]}
	for _, r := range runs[1:] {
		if last := &out[len(out)-1]; r.Start <= last.End {
			last.End = max(last.End, r.End)
		} else {
			out = append(out, r)
		}
	}
	return out
}

// Gaps of zero to total the runs leave
func missing(total int64, covered []Range) []Range {
	var gaps []Range
	var cursor int64
	for _, r := range coalesce(covered) {
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

func size(ranges []Range) int64 {
	var n int64
	for _, r := range ranges {
		n += r.End - r.Start
	}
	return n
}
