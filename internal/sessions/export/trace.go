package export

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/tools/redact"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Version the traces name as the tool that wrote them
const traceVersion = "icebath-agent"

// Branch the folder's git checkout is on, empty outside one
func gitBranch(dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	command.Dir = dir
	out, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Session as Claude Code JSONL, which the Hugging Face Agent Trace Viewer reads: one line per message the model saw, each pointing at the
// one before. Secrets are masked unless told not to
func Trace(t *sessionsv1.Transcript, mask bool) ([]byte, error) {
	s := t.GetSession()
	scrub := func(text string) string {
		if mask {
			return redact.File(text, true)
		}
		return text
	}
	branch := cmp.Or(s.GetGitBranch(), gitBranch(s.GetDirectory()))
	var b bytes.Buffer
	var parent any
	for _, m := range t.GetMessages() {
		if m.GetRole() == sessionsv1.Role_ROLE_SYSTEM || m.GetCompacted() {
			continue
		}
		kind, body := "user", map[string]any{}
		switch m.GetRole() {
		case sessionsv1.Role_ROLE_ASSISTANT:
			kind = "assistant"
			var content []map[string]any
			if text := scrub(m.GetText()); text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			for _, call := range m.GetToolCalls() {
				input := map[string]any{}
				arguments := scrub(call.GetArguments())
				if strings.TrimSpace(arguments) != "" {
					if err := json.Unmarshal([]byte(arguments), &input); err != nil {
						if mask && strings.TrimSpace(call.GetArguments()) != "" && json.Valid([]byte(call.GetArguments())) {
							return nil, errors.New("Redaction failed; refusing to export unredacted trace content.")
						}
						input = map[string]any{"_raw": arguments}
					}
				}
				id := call.GetId()
				if id == "" {
					id = "toolu_" + hex.EncodeToString([]byte(uuid.NewString()))[:16]
				}
				content = append(content, map[string]any{"type": "tool_use", "id": id, "name": cmp.Or(call.GetName(), "tool"), "input": input})
			}
			if len(content) == 0 {
				content = []map[string]any{{"type": "text", "text": ""}}
			}
			body = map[string]any{"role": "assistant", "model": cmp.Or(s.GetModel(), "unknown"), "content": content}
		case sessionsv1.Role_ROLE_TOOL:
			body = map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result",
				"tool_use_id": cmp.Or(m.GetToolCallId(), cmp.Or(m.GetToolName(), "tool")), "content": scrub(m.GetText())}}}
		default:
			var content any = scrub(m.GetText())
			if len(m.GetMedia()) > 0 {
				blocks := []map[string]any{{"type": "text", "text": scrub(m.GetText())}}
				for range m.GetMedia() {
					blocks = append(blocks, map[string]any{"type": "text", "text": "[image omitted]"})
				}
				content = blocks
			}
			body = map[string]any{"role": "user", "content": content}
		}
		at := time.Now()
		if m.GetTime() != nil {
			at = m.GetTime().AsTime()
		}
		id := uuid.NewString()
		line, err := encode(map[string]any{"parentUuid": parent, "isSidechain": false, "userType": "external", "cwd": s.GetDirectory(),
			"sessionId": s.GetId(), "version": traceVersion, "gitBranch": branch, "uuid": id, "timestamp": at.UTC().Format("2006-01-02T15:04:05.000Z"),
			"type": kind, "message": body})
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
		parent = id
	}
	return b.Bytes(), nil
}

// Hugging Face token of the environment, the first of its names set
func hfToken() string {
	for _, name := range []string{"HF_TOKEN", "HUGGINGFACE_HUB_TOKEN", "HUGGING_FACE_HUB_TOKEN", "HUGGINGFACE_TOKEN"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// Account of an upload the person reads, its link on success
func Upload(ctx context.Context, session string, trace []byte, public bool) string {
	token := hfToken()
	if token == "" {
		return "Can't upload — no Hugging Face token is available. To set it up:\n\n1. Create a token with WRITE access at " +
			"https://huggingface.co/settings/tokens\n   (New token -> type \"Write\" -> copy it).\n2. Add it to your environment as HF_TOKEN:\n     " +
			"HF_TOKEN=hf_xxxxxxxxxxxxxxxxxxxx\n3. Run `icebath sessions export --format trace --upload` again."
	}
	if len(trace) == 0 {
		return "No transcript content to upload for this session."
	}
	hub := &hfClient{endpoint: strings.TrimRight(cmp.Or(os.Getenv("HF_ENDPOINT"), "https://huggingface.co"), "/"), token: token}
	var who struct {
		Name string `json:"name"`
	}
	if err := hub.call(ctx, "GET", "/api/whoami-v2", nil, &who); err != nil {
		return "Your Hugging Face token was rejected (whoami failed). Make sure it has WRITE access and isn't expired."
	}
	if who.Name == "" {
		return "Could not resolve your Hugging Face username from the token."
	}
	repo := who.Name + "/icebath-traces"
	visibility := "private"
	if public {
		visibility = "public"
	}
	if err := hub.createRepo(ctx, who.Name, visibility); err != nil {
		if hub.call(ctx, "GET", "/api/datasets/"+repo, nil, nil) != nil {
			return fmt.Sprintf("Could not create/access dataset %s: %v", repo, err)
		}
	}
	path := "sessions/" + session + ".jsonl"
	skipped, err := hub.upload(ctx, repo, path, session, trace)
	if err != nil {
		return "Upload to Hugging Face failed: " + err.Error()
	}
	if skipped {
		return "No files have been modified since last commit. Skipping to prevent empty commit."
	}
	return fmt.Sprintf("Uploaded -> %s/datasets/%s/blob/main/%s\nView in the trace viewer: %s/datasets/%s", hub.endpoint, repo, path, hub.endpoint, repo)
}

type hfClient struct {
	endpoint, token string
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.code, strings.TrimSpace(e.body))
}

func (h *hfClient) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	return h.raw(ctx, method, h.endpoint+path, reader, "application/json", out)
}

func (h *hfClient) raw(ctx context.Context, method, url string, body io.Reader, contentType string, out any) error {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	request.Header.Set("authorization", "Bearer "+h.token)
	if body != nil {
		request.Header.Set("content-type", contentType)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 300 {
		return &statusError{code: response.StatusCode, body: string(data)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Makes the dataset of the account, a dataset already there being fine; a create racing another is tried again
func (h *hfClient) createRepo(ctx context.Context, user, visibility string) error {
	for attempt := 0; ; attempt++ {
		err := h.call(ctx, "POST", "/api/repos/create", map[string]any{"name": "icebath-traces", "organization": user, "visibility": visibility, "type": "dataset"}, nil)
		var status *statusError
		if err == nil || errors.As(err, &status) && status.code == 409 && !strings.Contains(status.body, "conflicting operation is in progress") {
			return nil
		}
		if errors.As(err, &status) && status.code == 409 && attempt < 5 {
			time.Sleep(time.Second << attempt)
			continue
		}
		return err
	}
}

// Uploads the file to the dataset in one commit, through large file storage when the hub asks; skipped when the hub already holds it
func (h *hfClient) upload(ctx context.Context, repo, path, session string, data []byte) (bool, error) {
	sample := data[:min(len(data), 512)]
	var pre struct {
		Files []struct {
			Path         string `json:"path"`
			UploadMode   string `json:"uploadMode"`
			ShouldIgnore bool   `json:"shouldIgnore"`
			OID          string `json:"oid"`
		} `json:"files"`
	}
	if err := h.call(ctx, "POST", "/api/datasets/"+repo+"/preupload/main", map[string]any{"files": []map[string]any{{"path": path,
		"sample": base64.StdEncoding.EncodeToString(sample), "size": len(data)}}}, &pre); err != nil {
		return false, err
	}
	mode, remote := "regular", ""
	if len(pre.Files) > 0 {
		mode, remote = cmp.Or(pre.Files[0].UploadMode, "regular"), pre.Files[0].OID
	}
	digest := sha256.Sum256(data)
	oid := hex.EncodeToString(digest[:])
	if mode == "regular" {
		blob := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...))
		if remote == hex.EncodeToString(blob[:]) {
			return true, nil
		}
	} else if remote == oid {
		return true, nil
	}
	if mode == "lfs" {
		if err := h.lfs(ctx, repo, oid, data); err != nil {
			return false, err
		}
	}
	var lines bytes.Buffer
	header, _ := json.Marshal(map[string]any{"key": "header", "value": map[string]any{"summary": "add session trace " + session, "description": ""}})
	lines.Write(append(header, '\n'))
	var file []byte
	if mode == "lfs" {
		file, _ = json.Marshal(map[string]any{"key": "lfsFile", "value": map[string]any{"path": path, "algo": "sha256", "oid": oid, "size": len(data)}})
	} else {
		file, _ = json.Marshal(map[string]any{"key": "file", "value": map[string]any{"content": base64.StdEncoding.EncodeToString(data), "path": path,
			"encoding": "base64"}})
	}
	lines.Write(append(file, '\n'))
	var commit struct {
		URL string `json:"commitUrl"`
		OID string `json:"commitOid"`
	}
	if err := h.raw(ctx, "POST", h.endpoint+"/api/datasets/"+repo+"/commit/main", &lines, "application/x-ndjson", &commit); err != nil {
		return false, err
	}
	if commit.URL == "" || commit.OID == "" {
		return false, errors.New("the hub did not confirm the commit")
	}
	return false, nil
}

// Uploads the file's content to large file storage before the commit names it
func (h *hfClient) lfs(ctx context.Context, repo, oid string, data []byte) error {
	var batch struct {
		Objects []struct {
			Actions map[string]struct {
				Href   string            `json:"href"`
				Header map[string]string `json:"header"`
			} `json:"actions"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"objects"`
	}
	request := map[string]any{"operation": "upload", "transfers": []string{"basic"}, "hash_algo": "sha256",
		"objects": []map[string]any{{"oid": oid, "size": len(data)}}}
	body, _ := json.Marshal(request)
	if err := h.raw(ctx, "POST", h.endpoint+"/datasets/"+repo+".git/info/lfs/objects/batch", bytes.NewReader(body), "application/vnd.git-lfs+json",
		&batch); err != nil {
		return err
	}
	if len(batch.Objects) == 0 {
		return nil
	}
	object := batch.Objects[0]
	if object.Error != nil {
		return errors.New(object.Error.Message)
	}
	if upload, ok := object.Actions["upload"]; ok {
		put, err := http.NewRequestWithContext(ctx, "PUT", upload.Href, bytes.NewReader(data))
		if err != nil {
			return err
		}
		for key, value := range upload.Header {
			put.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(put)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode >= 300 {
			return fmt.Errorf("large file upload answered HTTP %d", response.StatusCode)
		}
	}
	if verify, ok := object.Actions["verify"]; ok {
		body, _ := json.Marshal(map[string]any{"oid": oid, "size": len(data)})
		return h.raw(ctx, "POST", verify.Href, bytes.NewReader(body), "application/vnd.git-lfs+json", nil)
	}
	return nil
}
