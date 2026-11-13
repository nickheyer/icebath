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
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

const (
	bizCodeDailyLimit        = 40093002
	bizCodePartRetryable     = 40093001
	maxConcurrentParts       = 10
	partUploadTimeout        = 300 * time.Second
	partUploadMaxRetries     = 2
	partFinishRetryInterval  = time.Second
	partFinishDefaultTimeout = 120 * time.Second
	partFinishMaxTimeout     = 600 * time.Second
	completeUploadMaxRetries = 2
	completeUploadBaseDelay  = 2 * time.Second
	md5HeadSize              = 10_002_432
)

// Upload refused because the bot hit its daily cumulative upload quota
type dailyLimitError struct {
	fileName string
	fileSize int64
	cause    error
}

func (e *dailyLimitError) Error() string {
	return fmt.Sprintf("daily upload limit exceeded for %q: %v", e.fileName, e.cause)
}

func (e *dailyLimitError) Unwrap() error { return e.cause }

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
	retryTimeout time.Duration
}

// Reads an upload_prepare answer, bare or wrapped in data
func parsePrepareResponse(raw map[string]any) (prepareResult, error) {
	src := raw
	if d := jmap(raw["data"]); d != nil {
		src = d
	}
	uploadID := jstr(src["upload_id"])
	if uploadID == "" {
		return prepareResult{}, fmt.Errorf("upload_prepare response missing upload_id: %s", cutRunes(fmt.Sprint(raw), 200))
	}
	rawParts := jlist(src["parts"])
	if len(rawParts) == 0 {
		rawParts = jlist(src["part_list"])
	}
	if len(rawParts) == 0 {
		return prepareResult{}, fmt.Errorf("upload_prepare response missing parts: %s", cutRunes(fmt.Sprint(raw), 200))
	}
	res := prepareResult{uploadID: uploadID, concurrency: 1}
	res.blockSize, _ = jint(src["block_size"])
	for _, pv := range rawParts {
		p := jmap(pv)
		if p == nil {
			continue
		}
		index, ok := jint(p["part_index"])
		if !ok || index == 0 {
			index, _ = jint(p["index"])
		}
		u := jstr(p["presigned_url"])
		if u == "" {
			u = jstr(p["url"])
		}
		size, _ := jint(p["block_size"])
		res.parts = append(res.parts, preparePart{index: index, presignedURL: u, blockSize: size})
	}
	if c, ok := jint(src["concurrency"]); ok && c > 0 {
		res.concurrency = c
	}
	res.retryTimeout = time.Duration(jfloat(src["retry_timeout"], 0) * float64(time.Second))
	return res, nil
}

// Readable byte count with one decimal in B, KB, MB, GB or TB
func formatSize(sizeBytes int64) string {
	size := float64(sizeBytes)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if size < 1024.0 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
		size /= 1024.0
	}
	return fmt.Sprintf("%.1f TB", size)
}

// State one chunked upload shares across its part workers
type uploadJob struct {
	chatType, targetID, filePath string
	fileSize                     int64
	uploadID                     string
	blockSize                    int64
	totalParts                   int
	retryTimeout                 time.Duration
	completed                    atomic.Int64
}

func (j *uploadJob) path(endpoint string) string { return restPath(j.chatType, j.targetID, endpoint) }

// Prepares, PUTs every part to COS, then completes, returning the answer carrying file_info
func (r *restClient) chunkedUpload(ctx context.Context, chatType, targetID, filePath string, fileType int, fileName string) (map[string]any, error) {
	if chatType != "c2c" && chatType != "group" {
		return nil, fmt.Errorf("chunked upload does not apply to chat type %q", chatType)
	}
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}
	job := &uploadJob{chatType: chatType, targetID: targetID, filePath: filePath, fileSize: fi.Size()}
	slog.Info(fmt.Sprintf("[%s] Chunked upload start: file=%s size=%s type=%d", r.logTag, fileName, formatSize(job.fileSize), fileType))
	md5Full, sha1Full, md5Head, err := fileHashes(filePath, job.fileSize)
	if err != nil {
		return nil, err
	}
	raw, err := r.api(ctx, http.MethodPost, job.path("upload_prepare"), map[string]any{"file_type": fileType, "file_name": fileName,
		"file_size": job.fileSize, "md5": md5Full, "sha1": sha1Full, "md5_10m": md5Head}, fileUploadTimeout)
	if err != nil {
		if hasBizCode(err, bizCodeDailyLimit) {
			return nil, &dailyLimitError{fileName: fileName, fileSize: job.fileSize, cause: err}
		}
		return nil, err
	}
	prepare, err := parsePrepareResponse(raw)
	if err != nil {
		return nil, err
	}
	workers := int(min(prepare.concurrency, maxConcurrentParts))
	job.uploadID, job.blockSize, job.totalParts = prepare.uploadID, prepare.blockSize, len(prepare.parts)
	job.retryTimeout = partFinishDefaultTimeout
	if prepare.retryTimeout > 0 {
		job.retryTimeout = prepare.retryTimeout
	}
	job.retryTimeout = min(job.retryTimeout, partFinishMaxTimeout)
	slog.Info(fmt.Sprintf("[%s] Prepared: upload_id=%s block_size=%s parts=%d concurrency=%d", r.logTag, job.uploadID,
		formatSize(job.blockSize), job.totalParts, workers))
	if err := r.uploadParts(ctx, job, prepare.parts, workers); err != nil {
		return nil, err
	}
	slog.Info(fmt.Sprintf("[%s] All %d parts uploaded, completing", r.logTag, job.totalParts))
	return withRetries(ctx, r.logTag, completeUploadMaxRetries, completeUploadBaseDelay, "complete_upload", "complete_upload",
		func() (map[string]any, error) {
			return r.api(ctx, http.MethodPost, job.path("files"), map[string]any{"upload_id": job.uploadID}, fileUploadTimeout)
		})
}

// Uploads parts through a bounded worker pool, stopping the rest at the first failure
func (r *restClient) uploadParts(ctx context.Context, job *uploadJob, parts []preparePart, workers int) error {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan preparePart, len(parts))
	for _, p := range parts {
		queue <- p
	}
	close(queue)
	results := make(chan error, len(parts))
	for range min(max(workers, 1), len(parts)) {
		go func() {
			for p := range queue {
				if pctx.Err() != nil {
					results <- pctx.Err()
					continue
				}
				results <- r.uploadOnePart(pctx, job, p)
			}
		}()
	}
	var first error
	for range parts {
		if err := <-results; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

// PUTs one part to its presigned COS URL and then reports it finished
func (r *restClient) uploadOnePart(ctx context.Context, job *uploadJob, part preparePart) error {
	offset := (part.index - 1) * job.blockSize
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
	label := fmt.Sprintf("%d/%d", part.index, job.totalParts)
	slog.Debug(fmt.Sprintf("[%s] Part %s: uploading %s (offset=%d md5=%s)", r.logTag, label, formatSize(length), offset, md5Hex))
	if _, err := withRetries(ctx, r.logTag, partUploadMaxRetries, time.Second, "PUT part "+label, "Part "+label+" upload",
		func() (map[string]any, error) { return nil, r.putPart(ctx, part.presignedURL, data) }); err != nil {
		return err
	}
	if err := r.partFinish(ctx, job, part.index, length, md5Hex); err != nil {
		return err
	}
	done := job.completed.Add(1)
	slog.Debug(fmt.Sprintf("[%s] Part %s done (%d/%d total)", r.logTag, label, done, job.totalParts))
	return nil
}

// One PUT of part bytes to COS, an error for any non 2xx answer
func (r *restClient) putPart(ctx context.Context, presignedURL string, data []byte) error {
	status, raw, err := r.send(ctx, http.MethodPut, presignedURL, nil, data, partUploadTimeout)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("COS PUT returned %d: %s", status, cutRunes(string(raw), 200))
	}
	return nil
}

// Reports a part finished, retrying the transient business code until the job retry timeout
func (r *restClient) partFinish(ctx context.Context, job *uploadJob, partIndex, blockSize int64, md5Hex string) error {
	body := map[string]any{"upload_id": job.uploadID, "part_index": partIndex, "block_size": blockSize, "md5": md5Hex}
	start := time.Now()
	for attempt := 0; ; attempt++ {
		_, err := r.api(ctx, http.MethodPost, job.path("upload_part_finish"), body, fileUploadTimeout)
		if err == nil {
			return nil
		}
		if !hasBizCode(err, bizCodePartRetryable) {
			return err
		}
		elapsed := time.Since(start)
		if elapsed >= job.retryTimeout {
			return fmt.Errorf("upload_part_finish persistent retry timed out after %.0fs (%d retries): %w", job.retryTimeout.Seconds(), attempt, err)
		}
		slog.Debug(fmt.Sprintf("[%s] part_finish retryable error, attempt %d, elapsed=%.1fs: %v", r.logTag, attempt+1, elapsed.Seconds(), err))
		if err := sleepCtx(ctx, partFinishRetryInterval); err != nil {
			return err
		}
	}
}

// Runs the attempt up to maxRetries plus one times with exponential backoff between them
func withRetries(ctx context.Context, logTag string, maxRetries int, baseDelay time.Duration, label, failureLabel string, attempt func() (map[string]any, error)) (map[string]any, error) {
	var last error
	for i := 0; i <= maxRetries; i++ {
		result, err := attempt()
		if err == nil {
			return result, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if i < maxRetries {
			delay := baseDelay << i
			slog.Warn(fmt.Sprintf("[%s] %s attempt %d failed, retry in %.1fs: %v", logTag, label, i+1, delay.Seconds(), err))
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("%s failed after %d attempts: %w", failureLabel, maxRetries+1, last)
}

// Reads exactly length bytes at offset, failing on a truncated file
func readFileChunk(filePath string, offset, length int64) ([]byte, error) {
	fh, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	data := make([]byte, max(length, 0))
	n, err := fh.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(n) != length {
		return nil, fmt.Errorf("short read from %s: expected %d bytes at offset %d, got %d (file may be truncated)", filePath, length, offset, n)
	}
	return data, nil
}

// Full md5, full sha1 and md5 of the first 10002432 bytes in one pass, the head hash equal to md5 for small files
func fileHashes(filePath string, fileSize int64) (string, string, string, error) {
	fh, err := os.Open(filePath)
	if err != nil {
		return "", "", "", err
	}
	defer fh.Close()
	full, s1, head := md5.New(), sha1.New(), md5.New()
	if _, err := io.Copy(io.MultiWriter(full, s1), io.TeeReader(io.LimitReader(fh, md5HeadSize), head)); err != nil {
		return "", "", "", err
	}
	if _, err := io.Copy(io.MultiWriter(full, s1), fh); err != nil {
		return "", "", "", err
	}
	fullMD5 := hex.EncodeToString(full.Sum(nil))
	headMD5 := fullMD5
	if fileSize > md5HeadSize {
		headMD5 = hex.EncodeToString(head.Sum(nil))
	}
	return fullMD5, hex.EncodeToString(s1.Sum(nil)), headMD5, nil
}
