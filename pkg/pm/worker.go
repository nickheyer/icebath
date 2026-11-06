package pm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// PyError is a failure carrying the Python exception class callers and the worker wire tell apart
type PyError struct {
	Class   string
	Message string
}

func (e *PyError) Error() string { return e.Message }

// Exception classes a worker failure keeps on the caller side, every other class arriving as RuntimeError
var workerErrorClasses = map[string]bool{
	"ValueError": true, "TypeError": true, "KeyError": true, "OSError": true,
	"FileExistsError": true, "FileNotFoundError": true, "PermissionError": true,
}

var classPrefix = regexp.MustCompile(`^(KeyError|TypeError|ValueError|AttributeError|RuntimeError|OSError): `)

// Python class name of a failure as type of exc name spells it
func errorClass(err error) string {
	var conflict *ResolutionConflict
	if errors.As(err, &conflict) {
		return "ResolutionConflict"
	}
	var install *InstallError
	if errors.As(err, &install) {
		return "InstallError"
	}
	var download *DownloadError
	if errors.As(err, &download) {
		switch download.Kind {
		case DownloadWasPaused:
			return "DownloadPaused"
		case DownloadHashFailed:
			return "HashError"
		case DownloadRangeFailed:
			return "_RangeError"
		case DownloadTransportFailed:
			return "DownloadTransportError"
		}
		return "DownloadError"
	}
	var py *PyError
	if errors.As(err, &py) {
		return py.Class
	}
	var tar *TarReadError
	if errors.As(err, &tar) {
		return tar.Class
	}
	var unknown *UnknownPackageError
	if errors.As(err, &unknown) {
		return "KeyError"
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, fs.ErrExist):
		return "FileExistsError"
	case errors.Is(err, fs.ErrPermission):
		return "PermissionError"
	}
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) {
		return "OSError"
	}
	if m := classPrefix.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return "RuntimeError"
}

// Wire form of a failure, install failures carrying their package, cause and remedy
func errorPayload(err error) map[string]any {
	payload := map[string]any{"type": errorClass(err), "message": err.Error()}
	var install *InstallError
	if errors.As(err, &install) {
		payload["package"] = install.Package
		payload["cause"] = install.Cause
		payload["remedy"] = install.Remedy
	}
	return payload
}

// Caller side failure a worker error names, its class kept where callers distinguish it
func workerError(payload map[string]any) error {
	kind := pyrepr.Str(payload["type"])
	if _, ok := payload["package"]; ok {
		pkg, cause, remedy := pyrepr.Str(payload["package"]), pyrepr.Str(payload["cause"]), pyrepr.Str(payload["remedy"])
		if kind == "ResolutionConflict" {
			return &ResolutionConflict{&InstallError{Package: pkg, Cause: cause, Remedy: remedy}}
		}
		return &InstallError{Package: pkg, Cause: cause, Remedy: remedy}
	}
	message := pyrepr.Str(payload["message"])
	if kind == "DownloadPaused" {
		return downloadError(DownloadWasPaused, "%s", message)
	}
	if !workerErrorClasses[kind] {
		return &PyError{Class: "RuntimeError", Message: message}
	}
	if kind == "KeyError" {
		return &PyError{Class: kind, Message: pyrepr.StrRepr(message)}
	}
	return &PyError{Class: kind, Message: message}
}

// Unbounded control message queue between the reader and the worker, nil marking the client gone
type controlQueue struct {
	mu    sync.Mutex
	ready *sync.Cond
	items []any
}

// Empty control queue
func newControlQueue() *controlQueue {
	q := &controlQueue{}
	q.ready = sync.NewCond(&q.mu)
	return q
}

// Appends one message, error or the disconnect marker
func (q *controlQueue) put(item any) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	q.ready.Signal()
}

// Next message, a disconnect or the reader's failure
func (q *controlQueue) receive() (map[string]any, error) {
	q.mu.Lock()
	for len(q.items) == 0 {
		q.ready.Wait()
	}
	item := q.items[0]
	q.items = q.items[1:]
	q.mu.Unlock()
	switch m := item.(type) {
	case nil:
		return nil, &PyError{Class: "RuntimeError", Message: "PM client disconnected"}
	case error:
		return nil, m
	}
	return item.(map[string]any), nil
}

// Reads control lines, a cancel flag pausing the operation and every other message queued in order
func readControls(controls io.ReadCloser, messages *controlQueue, pause *PauseEvent) {
	defer func() {
		controls.Close()
		pause.Set()
		messages.put(nil)
	}()
	reader := bufio.NewReader(controls)
	var requestID any
	haveID := false
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				messages.put(err)
			}
			return
		}
		value, err := loadJSON(bytes.TrimSuffix(line, []byte("\n")))
		if err != nil {
			messages.put(&PyError{Class: "ValueError", Message: err.Error()})
			return
		}
		message, ok := value.(map[string]any)
		if !ok {
			return
		}
		id, ok := message["id"]
		if !ok {
			messages.put(&PyError{Class: "KeyError", Message: "'id'"})
			return
		}
		if !haveID {
			requestID, haveID = id, true
		}
		if !jsonEquals(id, requestID) {
			messages.put(&PyError{Class: "ValueError", Message: "unexpected PM control request id"})
			return
		}
		if pyrepr.Truthy(message["cancel"]) {
			pause.Set()
		}
		if !jsonEquals(message["type"], "cancel") {
			messages.put(message)
		}
	}
}

// Required key of a decoded request, the KeyError Python raises when it is missing
func requestField(m map[string]any, key string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, &PyError{Class: "KeyError", Message: pyrepr.StrRepr(key)}
	}
	return v, nil
}

// Package definitions a request carries
func requestDefinitions(value any) ([]PackageDefinition, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(value))}
	}
	out := make([]PackageDefinition, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: "package definition is not an object"}
		}
		out = append(out, PackageDefinition{Name: pyrepr.Str(m["name"]), Module: pyrepr.Str(m["module"]), Qualname: pyrepr.Str(m["qualname"])})
	}
	return out, nil
}

// JSON form of download coverage, destinations in report order with their covered runs
func rangesPayload(ranges []RangeSet) *OrderedObject {
	out := NewOrderedObject()
	for _, set := range ranges {
		rows := make([]any, len(set.Ranges))
		for i, r := range set.Ranges {
			rows[i] = []any{jsonInt64(r.Start), jsonInt64(r.End)}
		}
		out.Set(set.Key, rows)
	}
	return out
}

// JSON number of an int64
func jsonInt64(n int64) json.Number { return json.Number(strconv.FormatInt(n, 10)) }

// Serves one JSON line PM request read from controls, writing callbacks and the result to wire
func ServeWorker(ctx context.Context, controls io.ReadCloser, wire io.Writer) error {
	messages := newControlQueue()
	pause := NewPauseEvent()
	go readControls(controls, messages, pause)
	request, err := messages.receive()
	if err != nil {
		return err
	}
	rawContext, err := requestField(request, "context")
	if err != nil {
		return err
	}
	pathsContext, ok := rawContext.(map[string]any)
	if !ok {
		return &PyError{Class: "TypeError", Message: "request context is not an object"}
	}
	repo, err := requestField(pathsContext, "repo")
	if err != nil {
		return err
	}
	lockfile, err := requestField(pathsContext, "lockfile")
	if err != nil {
		return err
	}
	previousRepo, previousLock := repoRoot, lockfilePath
	repoRoot = func() string { return pyrepr.Str(repo) }
	lockfilePath = func() string { return pyrepr.Str(lockfile) }
	defer func() { repoRoot, lockfilePath = previousRepo, previousLock }()

	var wireMu sync.Mutex
	send := func(data map[string]any) error {
		message := NewOrderedObject()
		message.Set("id", request["id"])
		for _, key := range sortedKeys(data) {
			message.Set(key, data[key])
		}
		line, err := dumpsJSON(message, false)
		if err != nil {
			return err
		}
		wireMu.Lock()
		defer wireMu.Unlock()
		_, err = wire.Write(append(line, '\n'))
		return err
	}
	call := 0
	var callbackMu sync.Mutex
	callback := func(name string, args ...any) (any, error) {
		callbackMu.Lock()
		defer callbackMu.Unlock()
		call++
		if err := send(map[string]any{"type": "callback", "callback": name, "call": jsonInt(call), "args": args}); err != nil {
			return nil, err
		}
		reply, err := messages.receive()
		if err != nil {
			return nil, err
		}
		if !jsonEquals(reply["id"], request["id"]) || !jsonEquals(reply["call"], jsonInt(call)) {
			return nil, &PyError{Class: "RuntimeError", Message: "unexpected PM callback response"}
		}
		if message, failed := reply["error"]; failed {
			return nil, &PyError{Class: "RuntimeError", Message: pyrepr.Str(message)}
		}
		return reply["result"], nil
	}

	var updateID *string
	if id, ok := request["update_id"].(string); ok {
		updateID = &id
	}
	workerCtx := context.WithValue(ctx, receiptScopeKey{}, &receiptScope{})
	restore := WorkerContext(workerCtx, updateID)
	result, failure := func() (any, error) {
		definitions, err := requestDefinitions(request["packages"])
		if err != nil {
			return nil, err
		}
		if err := LoadPackageDefinitions(definitions); err != nil {
			return nil, err
		}
		rawOperation, err := requestField(request, "operation")
		if err != nil {
			return nil, err
		}
		operation := pyrepr.Str(rawOperation)
		spec, err := lookupOperation(operation)
		if err != nil {
			return nil, err
		}
		rawArguments, err := requestField(request, "arguments")
		if err != nil {
			return nil, err
		}
		arguments, ok := rawArguments.(map[string]any)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: operation + "() argument after ** must be a mapping"}
		}
		hooks := workerHooks{}
		if operation == "ensure" {
			hooks.pause = pause
		}
		names, _ := request["callbacks"].([]any)
		for _, rawName := range names {
			switch pyrepr.Str(rawName) {
			case "progress":
				hooks.progress = func(stage string, done, total int64, label string) error {
					_, err := callback("progress", stage, jsonInt64(done), jsonInt64(total), label)
					return err
				}
			case "download_progress":
				hooks.downloadProgress = func(done, total int64, ranges []RangeSet) error {
					_, err := callback("download_progress", jsonInt64(done), jsonInt64(total), rangesPayload(ranges))
					return err
				}
			}
		}
		return spec.run(workerCtx, arguments, hooks)
	}()
	response := map[string]any{"type": "result"}
	if failure != nil {
		response["error"] = errorPayload(failure)
	} else {
		response["result"] = result
	}
	if completed := LastCompletedObject(workerCtx); completed != nil {
		response["receipt"] = completed
	} else {
		response["receipt"] = nil
	}
	restore()
	return send(response)
}

// Runs this process as the isolated PM worker over its standard streams and returns its exit status
func WorkerMain() int {
	controls, wire, err := workerStdio()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer wire.Close()
	if err := ServeWorker(context.Background(), controls, wire); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", errorClass(err), err)
		return 1
	}
	return 0
}
