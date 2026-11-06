package qqbot

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	bizCodeDailyLimit        = 40093002
	bizCodePartRetryable     = 40093001
	defaultConcurrentParts   = 1
	maxConcurrentParts       = 10
	partUploadTimeout        = 300 * time.Second
	partUploadMaxRetries     = 2
	partFinishRetryInterval  = time.Second
	partFinishDefaultTimeout = 120.0
	partFinishMaxTimeout     = 600.0
	completeUploadMaxRetries = 2
	completeUploadBaseDelay  = 2.0
	md510MSize               = 10_002_432
)

// RuntimeError is the RuntimeError the adapter api request raises with the biz_code in its message
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string { return e.Msg }

func isRuntimeError(err error) bool {
	var rt *RuntimeError
	return errors.As(err, &rt)
}

// UploadError carries the file name and size of a failed upload
type UploadError struct {
	FileName string
	FileSize int64
	Message  string
}

func (e *UploadError) Error() string { return e.Message }

// FileSizeHuman is the readable file size
func (e *UploadError) FileSizeHuman() string { return FormatSize(e.FileSize) }

// UploadDailyLimitExceededError is raised when upload_prepare returns biz_code 40093002
type UploadDailyLimitExceededError struct{ UploadError }

// NewUploadDailyLimitExceededError defaults the message to the daily limit text
func NewUploadDailyLimitExceededError(fileName string, fileSize int64, message string) *UploadDailyLimitExceededError {
	if message == "" {
		message = fmt.Sprintf("Daily upload limit exceeded for %s", pyrepr.Repr(fileName))
	}
	return &UploadDailyLimitExceededError{UploadError{fileName, fileSize, message}}
}

// UploadFileTooLargeError is raised when a file exceeds the platform per file size limit
type UploadFileTooLargeError struct {
	UploadError
	LimitBytes int64
}

// NewUploadFileTooLargeError defaults the message to the size and the optional limit
func NewUploadFileTooLargeError(fileName string, fileSize, limitBytes int64, message string) *UploadFileTooLargeError {
	if message == "" {
		limitStr := ""
		if limitBytes != 0 {
			limitStr = " (" + FormatSize(limitBytes) + ")"
		}
		message = fmt.Sprintf("File %s (%s) exceeds platform limit%s", pyrepr.Repr(fileName), FormatSize(fileSize), limitStr)
	}
	return &UploadFileTooLargeError{UploadError{fileName, fileSize, message}, limitBytes}
}

// LimitHuman is the readable limit or unknown
func (e *UploadFileTooLargeError) LimitHuman() string {
	if e.LimitBytes != 0 {
		return FormatSize(e.LimitBytes)
	}
	return "unknown"
}

type preparePart struct {
	index        int64
	presignedURL string
	blockSize    int64
}

type prepareResult struct {
	uploadID     string
	blockSize    int64
	parts        []preparePart
	concurrency  int64
	retryTimeout float64
}

func reprHead(v any) string {
	r := []rune(pyrepr.Str(v))
	return string(r[:min(200, len(r))])
}

// Parses an upload_prepare response, bare or wrapped in data
func parsePrepareResponse(rawValue any) (prepareResult, error) {
	raw, ok := asDict(rawValue)
	if !ok {
		return prepareResult{}, notDictErr(rawValue)
	}
	src := raw
	if d, ok := asDict(raw["data"]); ok {
		src = d
	}
	uploadID := pyrepr.Str(getOr(src, "upload_id", ""))
	if uploadID == "" {
		return prepareResult{}, fmt.Errorf("upload_prepare response missing upload_id: %s", reprHead(rawValue))
	}
	blockSize, err := pyInt64(getOr(src, "block_size", int64(0)))
	if err != nil {
		return prepareResult{}, err
	}
	rawParts, ok := orDefault(orDefault(src["parts"], src["part_list"]), []any{}).([]any)
	if !ok || len(rawParts) == 0 {
		return prepareResult{}, fmt.Errorf("upload_prepare response missing parts: %s", reprHead(rawValue))
	}
	var parts []preparePart
	for _, pv := range rawParts {
		p, ok := asDict(pv)
		if !ok {
			continue
		}
		index, err := pyInt64(orDefault(orDefault(p["part_index"], p["index"]), int64(0)))
		if err != nil {
			return prepareResult{}, err
		}
		url := pyrepr.Str(orDefault(orDefault(p["presigned_url"], p["url"]), ""))
		size, err := pyInt64(getOr(p, "block_size", int64(0)))
		if err != nil {
			return prepareResult{}, err
		}
		parts = append(parts, preparePart{index, url, size})
	}
	concurrency, err := pyInt64(getOr(src, "concurrency", int64(defaultConcurrentParts)))
	if err != nil {
		return prepareResult{}, err
	}
	if concurrency == 0 {
		concurrency = defaultConcurrentParts
	}
	retryTimeout, err := pyint.Float(orDefault(getOr(src, "retry_timeout", 0.0), 0.0))
	if err != nil {
		return prepareResult{}, err
	}
	return prepareResult{uploadID, blockSize, parts, concurrency, retryTimeout}, nil
}

func apiPath(chatType, targetID, endpoint string) string {
	base := "/v2/groups"
	if chatType == "c2c" {
		base = "/v2/users"
	}
	return base + "/" + targetID + "/" + endpoint
}

type uploadJob struct {
	chatType, targetID, filePath string
	fileSize                     int64
	uploadID                     string
	blockSize                    int64
	totalParts                   int
	retryTimeout                 float64
	mu                           sync.Mutex
	completed                    int
}

func (j *uploadJob) path(endpoint string) string { return apiPath(j.chatType, j.targetID, endpoint) }

// APIRequestFunc is the adapter api request, failing with a RuntimeError that names the biz_code
type APIRequestFunc func(ctx context.Context, method, path string, body *pyjson.Object, timeout float64) (any, error)

// PutResponse is the status and body text of a COS PUT
type PutResponse struct {
	StatusCode int
	Text       string
}

// HTTPPutFunc PUTs part bytes to a presigned COS url
type HTTPPutFunc func(ctx context.Context, url string, data []byte, headers map[string]string) (PutResponse, error)

// ChunkedUploader runs the prepare, PUT parts and complete sequence of a QQ chunked upload
type ChunkedUploader struct {
	apiRequest APIRequestFunc
	httpPut    HTTPPutFunc
	logTag     string
}

// NewChunkedUploader binds the adapter api request and COS PUT functions under a log tag
func NewChunkedUploader(apiRequest APIRequestFunc, httpPut HTTPPutFunc, logTag string) *ChunkedUploader {
	return &ChunkedUploader{apiRequest: apiRequest, httpPut: httpPut, logTag: logTag}
}

// Swappable asyncio.sleep honoring cancellation
var uploadSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (u *ChunkedUploader) post(ctx context.Context, job *uploadJob, endpoint string, body *pyjson.Object) (any, error) {
	return u.apiRequest(ctx, "POST", job.path(endpoint), body, FileUploadTimeout)
}

// Upload runs the whole chunked upload and returns the raw complete_upload response carrying file_info
func (u *ChunkedUploader) Upload(ctx context.Context, chatType, targetID, filePath string, fileType int, fileName string) (any, error) {
	if chatType != "c2c" && chatType != "group" {
		return nil, fmt.Errorf("ChunkedUploader: unsupported chat_type %s", pyrepr.Repr(chatType))
	}
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}
	job := &uploadJob{chatType: chatType, targetID: targetID, filePath: filePath, fileSize: fi.Size()}
	slog.Info(fmt.Sprintf("[%s] Chunked upload start: file=%s size=%s type=%d", u.logTag, fileName, FormatSize(job.fileSize), fileType))
	hashes, err := computeFileHashes(filePath, job.fileSize)
	if err != nil {
		return nil, err
	}
	prepare, err := u.prepare(ctx, job, fileType, fileName, hashes)
	if err != nil {
		return nil, err
	}
	maxConcurrent := min(prepare.concurrency, maxConcurrentParts)
	job.uploadID, job.blockSize, job.totalParts = prepare.uploadID, prepare.blockSize, len(prepare.parts)
	retry := prepare.retryTimeout
	if retry <= 0 {
		retry = partFinishDefaultTimeout
	}
	job.retryTimeout = math.Min(retry, partFinishMaxTimeout)
	slog.Info(fmt.Sprintf("[%s] Prepared: upload_id=%s block_size=%s parts=%d concurrency=%d", u.logTag, job.uploadID, FormatSize(job.blockSize), job.totalParts, maxConcurrent))
	if err := u.uploadParts(ctx, job, prepare.parts, int(max(maxConcurrent, 1))); err != nil {
		return nil, err
	}
	slog.Info(fmt.Sprintf("[%s] All %d parts uploaded, completing…", u.logTag, job.totalParts))
	return u.complete(ctx, job)
}

// Uploads parts in order through a bounded worker pool and returns the first failure as soon as it happens
func (u *ChunkedUploader) uploadParts(ctx context.Context, job *uploadJob, parts []preparePart, workers int) error {
	queue := make(chan preparePart, len(parts))
	for _, p := range parts {
		queue <- p
	}
	close(queue)
	results := make(chan error, len(parts))
	for range min(workers, len(parts)) {
		go func() {
			for p := range queue {
				results <- u.uploadOnePart(ctx, job, p)
			}
		}()
	}
	for range parts {
		if err := <-results; err != nil {
			return err
		}
	}
	return nil
}

func (u *ChunkedUploader) prepare(ctx context.Context, job *uploadJob, fileType int, fileName string, hashes map[string]string) (prepareResult, error) {
	body := pyjson.NewObject()
	body.Set("file_type", int64(fileType))
	body.Set("file_name", fileName)
	body.Set("file_size", job.fileSize)
	body.Set("md5", hashes["md5"])
	body.Set("sha1", hashes["sha1"])
	body.Set("md5_10m", hashes["md5_10m"])
	raw, err := u.post(ctx, job, "upload_prepare", body)
	if err != nil {
		if isRuntimeError(err) && strings.Contains(err.Error(), strconv.Itoa(bizCodeDailyLimit)) {
			return prepareResult{}, NewUploadDailyLimitExceededError(fileName, job.fileSize, err.Error())
		}
		return prepareResult{}, err
	}
	return parsePrepareResponse(raw)
}

// PUTs one part to COS and then calls upload_part_finish
func (u *ChunkedUploader) uploadOnePart(ctx context.Context, job *uploadJob, part preparePart) error {
	partIndex, totalParts := part.index, job.totalParts
	offset := (partIndex - 1) * job.blockSize
	size := job.blockSize
	if part.blockSize > 0 {
		size = part.blockSize
	}
	length := min(size, job.fileSize-offset)
	data, err := readFileChunk(job.filePath, offset, length)
	if err != nil {
		return err
	}
	sum := md5.Sum(data)
	md5Hex := hex.EncodeToString(sum[:])
	slog.Debug(fmt.Sprintf("[%s] Part %d/%d: uploading %s (offset=%d md5=%s)", u.logTag, partIndex, totalParts, FormatSize(length), offset, md5Hex))
	if err := u.putToPresignedURL(ctx, part.presignedURL, data, partIndex, totalParts); err != nil {
		return err
	}
	if err := u.partFinishWithRetry(ctx, job, partIndex, length, md5Hex); err != nil {
		return err
	}
	job.mu.Lock()
	job.completed++
	completed := job.completed
	job.mu.Unlock()
	slog.Debug(fmt.Sprintf("[%s] Part %d/%d done (%d/%d total)", u.logTag, partIndex, totalParts, completed, totalParts))
	return nil
}

// Runs the attempt up to maxRetries plus one times with exponential backoff
func (u *ChunkedUploader) withRetries(ctx context.Context, attempt func() (any, error), maxRetries int, baseDelay float64, label, failureLabel string) (any, error) {
	var lastErr error
	for i := 0; i <= maxRetries; i++ {
		result, err := attempt()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if i < maxRetries {
			delay := baseDelay * math.Pow(2, float64(i))
			slog.Warn(fmt.Sprintf("[%s] %s attempt %d failed, retry in %.1fs: %v", u.logTag, label, i+1, delay, err))
			if err := uploadSleep(ctx, time.Duration(delay*float64(time.Second))); err != nil {
				return nil, err
			}
		}
	}
	return nil, &RuntimeError{fmt.Sprintf("%s failed after %d attempts: %v", failureLabel, maxRetries+1, lastErr)}
}

// PUT with the asyncio.wait_for timeout, whose TimeoutError renders as empty text
func (u *ChunkedUploader) putWithTimeout(ctx context.Context, url string, data []byte) (PutResponse, error) {
	pctx, cancel := context.WithTimeout(ctx, partUploadTimeout)
	defer cancel()
	type result struct {
		resp PutResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := u.httpPut(pctx, url, data, map[string]string{"Content-Length": strconv.Itoa(len(data))})
		done <- result{resp, err}
	}()
	select {
	case r := <-done:
		return r.resp, r.err
	case <-pctx.Done():
		if ctx.Err() != nil {
			return PutResponse{}, ctx.Err()
		}
		return PutResponse{}, errors.New("")
	}
}

func (u *ChunkedUploader) putToPresignedURL(ctx context.Context, url string, data []byte, partIndex int64, totalParts int) error {
	attempt := func() (any, error) {
		resp, err := u.putWithTimeout(ctx, url, data)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			slog.Debug(fmt.Sprintf("[%s] PUT part %d/%d: %d OK", u.logTag, partIndex, totalParts, resp.StatusCode))
			return nil, nil
		}
		preview := []rune(resp.Text)
		return nil, &RuntimeError{fmt.Sprintf("COS PUT returned %d: %s", resp.StatusCode, string(preview[:min(200, len(preview))]))}
	}
	_, err := u.withRetries(ctx, attempt, partUploadMaxRetries, 1.0,
		fmt.Sprintf("PUT part %d/%d", partIndex, totalParts), fmt.Sprintf("Part %d/%d upload", partIndex, totalParts))
	return err
}

// Calls upload_part_finish, retrying biz_code 40093001 until the job retry timeout
func (u *ChunkedUploader) partFinishWithRetry(ctx context.Context, job *uploadJob, partIndex, blockSize int64, md5Hex string) error {
	body := pyjson.NewObject()
	body.Set("upload_id", job.uploadID)
	body.Set("part_index", partIndex)
	body.Set("block_size", blockSize)
	body.Set("md5", md5Hex)
	start, attempt := time.Now(), 0
	for {
		_, err := u.post(ctx, job, "upload_part_finish", body)
		if err == nil {
			return nil
		}
		if !isRuntimeError(err) || !strings.Contains(err.Error(), strconv.Itoa(bizCodePartRetryable)) {
			return err
		}
		elapsed := time.Since(start).Seconds()
		if elapsed >= job.retryTimeout {
			return &RuntimeError{fmt.Sprintf("upload_part_finish persistent retry timed out after %ss (%d retries): %v",
				strconv.FormatFloat(job.retryTimeout, 'f', 0, 64), attempt, err)}
		}
		attempt++
		slog.Debug(fmt.Sprintf("[%s] part_finish retryable error, attempt %d, elapsed=%.1fs: %v", u.logTag, attempt, elapsed, err))
		if err := uploadSleep(ctx, partFinishRetryInterval); err != nil {
			return err
		}
	}
}

// Calls complete_upload on the files endpoint with retry
func (u *ChunkedUploader) complete(ctx context.Context, job *uploadJob) (any, error) {
	return u.withRetries(ctx, func() (any, error) {
		body := pyjson.NewObject()
		body.Set("upload_id", job.uploadID)
		return u.post(ctx, job, "files", body)
	}, completeUploadMaxRetries, completeUploadBaseDelay, "complete_upload", "complete_upload")
}

// FormatSize renders a byte count with one decimal in B, KB, MB, GB or TB
func FormatSize(sizeBytes int64) string {
	size := float64(sizeBytes)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if size < 1024.0 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
		size /= 1024.0
	}
	return fmt.Sprintf("%.1f TB", size)
}

// Reads length bytes at offset, a negative length reading to EOF, failing on a short read
func readFileChunk(filePath string, offset, length int64) ([]byte, error) {
	fh, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	if _, err := fh.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	var data []byte
	if length < 0 {
		data, err = io.ReadAll(fh)
	} else {
		data = make([]byte, length)
		var n int
		n, err = io.ReadFull(fh, data)
		data = data[:n]
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != length {
		return nil, fmt.Errorf("Short read from %s: expected %d bytes at offset %d, got %d (file may be truncated)", filePath, length, offset, len(data))
	}
	return data, nil
}

// Computes md5, sha1 and the md5 of the first 10002432 bytes in one pass
func computeFileHashes(filePath string, fileSize int64) (map[string]string, error) {
	fh, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	full, s1, head := md5.New(), sha1.New(), md5.New()
	need10M := fileSize > md510MSize
	var read int64
	buf := make([]byte, 65536)
	for {
		n, rerr := fh.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			full.Write(chunk)
			s1.Write(chunk)
			if remaining := md510MSize - read; need10M && remaining > 0 {
				head.Write(chunk[:min(int64(n), remaining)])
			}
			read += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	fullMD5 := hex.EncodeToString(full.Sum(nil))
	md510m := fullMD5
	if need10M {
		md510m = hex.EncodeToString(head.Sum(nil))
	}
	return map[string]string{"md5": fullMD5, "sha1": hex.EncodeToString(s1.Sum(nil)), "md5_10m": md510m}, nil
}
