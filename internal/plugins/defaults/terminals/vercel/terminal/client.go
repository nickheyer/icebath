package terminal

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
)

const api = "https://api.vercel.com"

// Statuses worth another try
var transient = map[int]bool{408: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// A reply of the API that is not a success
type apiError struct {
	Status int
	Body   string
	Code   string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("Vercel API HTTP %d (%s): %s", e.Status, e.Code, e.Body)
	}
	return fmt.Sprintf("Vercel API HTTP %d: %s", e.Status, e.Body)
}

// Whether the failure looks passing: a transient status or a network failure
func passing(err error) bool {
	if apiErr, ok := errors.AsType[*apiError](err); ok {
		return transient[apiErr.Status]
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// Whether the API answered that the thing asked for does not exist
func notFound(err error) bool {
	apiErr, ok := errors.AsType[*apiError](err)
	return ok && apiErr.Status == http.StatusNotFound
}

// Calls again after a passing failure, up to attempts with a growing pause
func retry[T any](label string, attempts int, call func() (T, error)) (T, error) {
	for attempt := 1; ; attempt++ {
		out, err := call()
		if err == nil || attempt >= attempts || !passing(err) {
			return out, err
		}
		slog.Warn(fmt.Sprintf("Vercel: %s failed; retrying", label), "error", err, "attempt", attempt, "attempts", attempts)
		time.Sleep(time.Duration(100*attempt) * time.Millisecond)
	}
}

// The token and the team and project it acts for
type credentials struct {
	token, team, project string
}

// Client of the sandbox REST API for one team
type client struct {
	credentials
	http *http.Client
}

// Request with the bearer token and the team, a reply that is not a success read into an error
func (c *client) do(ctx context.Context, method, p string, body []byte, contentType string, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	u, err := url.Parse(api + p)
	if err != nil {
		return nil, err
	}
	query := u.Query()
	query.Set("teamId", c.team)
	u.RawQuery = query.Encode()
	cancel := context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("User-Agent", provider.UserAgent("icebath"))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := c.http.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = &cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	if response.StatusCode < 400 {
		return response, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	response.Body.Close()
	failure := &apiError{Status: response.StatusCode, Body: strings.TrimSpace(string(raw))}
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		failure.Code = parsed.Error.Code
		if parsed.Error.Message != "" {
			failure.Body = parsed.Error.Message
		}
	}
	return nil, failure
}

// Body that ends the request's time limit once read
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// JSON request decoding its reply into out, which may be nil
func (c *client) json(ctx context.Context, method, p string, payload, out any, timeout time.Duration) error {
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	response, err := c.do(ctx, method, p, body, "application/json", nil, timeout)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("Vercel API returned non JSON: %s", strings.TrimSpace(string(raw)))
	}
	return nil
}

// The running VM of a sandbox
type session struct {
	ID     string `json:"id"`
	Cwd    string `json:"cwd"`
	Status string `json:"status"`
}

// Whether a session's status is final
func ended(status string) bool {
	return status == "aborted" || status == "failed" || status == "stopped"
}

// What a sandbox is made with
type createParams struct {
	// Longest the sandbox lives
	timeout time.Duration
	// Runtime, which takes the v2 endpoint, or the image
	runtime, image string
	vcpus, memory  int
}

// Makes a session, from the snapshot when one is named
func (c *client) create(ctx context.Context, p createParams, snapshot string) (session, error) {
	payload := map[string]any{"projectId": c.project, "timeout": p.timeout.Milliseconds()}
	resources := map[string]any{}
	if p.vcpus > 0 {
		resources["vcpus"] = p.vcpus
	}
	if p.memory > 0 {
		resources["memory"] = p.memory
	}
	if len(resources) > 0 {
		payload["resources"] = resources
	}
	endpoint := "/v3/sandboxes"
	switch {
	case snapshot != "":
		payload["source"] = map[string]any{"type": "snapshot", "snapshotId": snapshot}
	case p.runtime != "":
		payload["runtime"] = p.runtime
		endpoint = "/v2/sandboxes"
	case p.image != "":
		payload["image"] = p.image
	}
	var reply struct {
		Session session `json:"session"`
	}
	if err := c.json(ctx, "POST", endpoint, payload, &reply, 120*time.Second); err != nil {
		return session{}, err
	}
	if reply.Session.ID == "" {
		return session{}, errors.New("Vercel sandbox create did not return a session id")
	}
	return reply.Session, nil
}

func (c *client) get(ctx context.Context, id string) (session, error) {
	var reply struct {
		Session session `json:"session"`
	}
	err := c.json(ctx, "GET", "/v2/sandboxes/sessions/"+id, nil, &reply, 30*time.Second)
	return reply.Session, err
}

func (c *client) stop(ctx context.Context, id string) error {
	return c.json(ctx, "POST", "/v2/sandboxes/sessions/"+id+"/stop", nil, nil, 60*time.Second)
}

// Snapshot id of the running session's filesystem, empty when the reply names none
func (c *client) snapshot(ctx context.Context, id string) (string, error) {
	var reply map[string]any
	if err := c.json(ctx, "POST", "/v2/sandboxes/sessions/"+id+"/snapshot", nil, &reply, 120*time.Second); err != nil {
		return "", err
	}
	nested, _ := reply["snapshot"].(map[string]any)
	for _, fields := range []map[string]any{nested, reply} {
		for _, key := range []string{"snapshot_id", "snapshotId", "id"} {
			if id, ok := fields[key].(string); ok && id != "" {
				return id, nil
			}
		}
	}
	return "", nil
}

// One file to write into a sandbox
type file struct {
	path    string
	content []byte
	mode    int64
}

// Writes the files as one gzipped tar unpacked at the root
func (c *client) write(ctx context.Context, id string, files []file) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: strings.TrimLeft(path.Clean(f.path), "/"), Mode: mode, Size: int64(len(f.content)), ModTime: time.Now(), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if _, err := tw.Write(f.content); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	response, err := c.do(ctx, "POST", "/v2/sandboxes/sessions/"+id+"/fs/write", buf.Bytes(), "application/gzip", map[string]string{"x-cwd": "/"}, 120*time.Second)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

// Bytes of one file of the sandbox
func (c *client) read(ctx context.Context, id, p string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"path": p})
	if err != nil {
		return nil, err
	}
	response, err := c.do(ctx, "POST", "/v2/sandboxes/sessions/"+id+"/fs/read", body, "application/json", nil, 120*time.Second)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

// One line of a command's stream: the command as it starts and ends, or a piece of its output
type chunk struct {
	Command *struct {
		ID       string `json:"id"`
		ExitCode *int   `json:"exitCode"`
	} `json:"command"`
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// Runs a command to its end over the ND-JSON stream, answering its output in the order it arrived and its exit code
func (c *client) run(ctx context.Context, id, command string, args []string, cwd string, env map[string]string) (string, int, error) {
	if env == nil {
		env = map[string]string{}
	}
	body, err := json.Marshal(map[string]any{"command": command, "args": args, "cwd": cwd, "env": env, "wait": true, "logs": true})
	if err != nil {
		return "", 0, err
	}
	response, err := c.do(ctx, "POST", "/v2/sandboxes/sessions/"+id+"/cmd", body, "application/json", nil, 0)
	if err != nil {
		return "", 0, err
	}
	defer response.Body.Close()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/x-ndjson") {
		return "", 0, errors.New("Expected a stream of command data")
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var output strings.Builder
	commandID, started := "", false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		var piece chunk
		if len(line) == 0 || json.Unmarshal(line, &piece) != nil {
			continue
		}
		if piece.Command != nil {
			if !started {
				started, commandID = true, piece.Command.ID
				continue
			}
			if piece.Command.ExitCode != nil {
				return output.String(), *piece.Command.ExitCode, nil
			}
			continue
		}
		if piece.Stream == "error" {
			var failure struct {
				Message string `json:"message"`
			}
			json.Unmarshal(piece.Data, &failure)
			return "", 0, fmt.Errorf("Vercel sandbox stream error: %s", failure.Message)
		}
		var text string
		if json.Unmarshal(piece.Data, &text) == nil {
			output.WriteString(text)
		}
	}
	if !started || commandID == "" {
		return "", 0, errors.New("Stream ended before command data was received")
	}
	var reply struct {
		Command struct {
			ExitCode *int `json:"exitCode"`
		} `json:"command"`
	}
	if err := c.json(ctx, "GET", "/v2/sandboxes/sessions/"+id+"/cmd/"+commandID+"?wait=true", nil, &reply, 0); err != nil {
		return "", 0, err
	}
	code := 1
	if reply.Command.ExitCode != nil {
		code = *reply.Command.ExitCode
	}
	return output.String(), code, nil
}
