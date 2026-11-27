package skills

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Publishes a skill, by its path in the skills folder or an absolute one, to a registry once its scan finds nothing dangerous; false
// when it was not published
func (h *Hub) Publish(ctx context.Context, scope *skills.Scope, skillPath, target, repo string, say Say) (bool, error) {
	r := h.run(scope, say)
	dir := skillPath
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(scope.Dir(), dir)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if errors.Is(err, fs.ErrNotExist) {
		r.fail("There is no SKILL.md in %s.", dir)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !utf8.Valid(raw) {
		return false, errors.New("SKILL.md is not valid UTF-8")
	}
	fm, _ := skills.Parse(string(bytes.TrimPrefix(raw, []byte("\uFEFF"))))
	name := filepath.Base(dir)
	if v, ok := fm["name"]; ok && v != nil {
		name = fmt.Sprint(v)
	}
	if !config.Truthy(fm["description"]) {
		r.fail("The SKILL.md needs a description: line in its front matter.")
		return false, nil
	}
	r.line(Heading, "Scanning %s for risks before it is published…", name)
	result, _, err := r.scan(ctx, dir, "self", "", "")
	if err != nil {
		return false, err
	}
	r.line(Plain, report(result))
	if result.Verdict == "dangerous" {
		r.line(Bad, "✗ The scan calls the skill dangerous, so it was not published.")
		return false, nil
	}
	switch target {
	case "github":
		if repo == "" {
			r.fail("Publishing to GitHub needs --repo, such as icebath skills publish <skill_path> --to github --repo owner/repo.")
			return false, nil
		}
		if !r.gh.auth.signedIn(ctx) {
			r.fail("Publishing to GitHub needs a sign-in. Set GITHUB_TOKEN or GitHub token in /settings under Skills › Skills hub, or run gh auth login.")
			return false, nil
		}
		r.line(Heading, "Publishing %s to %s…", name, repo)
		message, ok := r.publish(ctx, dir, name, repo)
		if !ok {
			r.fail("%s", message)
			return false, nil
		}
		r.line(Good, message)
		return true, nil
	case "clawhub":
		r.line(Warn, "⚠ icebath can't publish to ClawHub yet. Submit the skill yourself at https://clawhub.ai/submit.")
		return true, nil
	}
	r.line(Bad, "✗ %s is not a registry icebath publishes to. Use github or clawhub.", target)
	return false, nil
}

// One call of the GitHub API, its status and body
func (r *run) githubCall(ctx context.Context, method, path string, timeout time.Duration, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, githubAPI+"/"+path, body)
	if err != nil {
		return 0, nil, err
	}
	for name, value := range r.gh.auth.headers(ctx) {
		req.Header.Set(name, value)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.follow.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, data, err
}

// Forks the repository, makes a branch, uploads the skill's files under skills/<name> and opens a pull request; what happened, as the
// user reads it
func (r *run) publish(ctx context.Context, dir, name, target string) (string, bool) {
	status, body, err := r.githubCall(ctx, http.MethodPost, target+"/forks", 30*time.Second, nil)
	if err != nil {
		return "The repository could not be forked, because GitHub could not be reached: " + strings.TrimSuffix(err.Error(), ".") + ".", false
	}
	var fork struct {
		FullName string `json:"full_name"`
	}
	switch status {
	case 200, 202:
		if json.Unmarshal(body, &fork) != nil || fork.FullName == "" {
			return "GitHub answered the fork without naming the new repository.", false
		}
	case 403:
		return "Your GitHub token may not fork repositories. Give it that right, or use another token.", false
	default:
		return fmt.Sprintf("%s could not be forked. GitHub answered with status %d.", target, status), false
	}
	branch := "main"
	if status, body, err := r.githubCall(ctx, http.MethodGet, target, 15*time.Second, nil); err == nil && status == 200 {
		var info struct {
			DefaultBranch string `json:"default_branch"`
		}
		if json.Unmarshal(body, &info) == nil && info.DefaultBranch != "" {
			branch = info.DefaultBranch
		}
	}
	status, body, err = r.githubCall(ctx, http.MethodGet, fork.FullName+"/git/refs/heads/"+branch, 15*time.Second, nil)
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err == nil && (status != 200 || json.Unmarshal(body, &ref) != nil || ref.Object.SHA == "") {
		err = fmt.Errorf("GitHub answered %d", status)
	}
	if err != nil {
		return "The main branch of the fork could not be read: " + strings.TrimSuffix(err.Error(), ".") + ".", false
	}
	head := "add-skill-" + name
	status, body, err = r.githubCall(ctx, http.MethodPost, fork.FullName+"/git/refs", 15*time.Second, map[string]string{"ref": "refs/heads/" + head, "sha": ref.Object.SHA})
	if err == nil && status != 201 && status != 422 {
		err = fmt.Errorf("GitHub answered %d %s", status, strs.Head(string(body), 200))
	}
	if err != nil {
		return "The branch for the skill could not be made: " + strings.TrimSuffix(err.Error(), ".") + ".", false
	}
	for _, rel := range walkFiles(dir) {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err == nil {
			status, body, err = r.githubCall(ctx, http.MethodPut, fmt.Sprintf("%s/contents/skills/%s/%s", fork.FullName, name, escapePath(rel)), 15*time.Second,
				map[string]string{"message": fmt.Sprintf("Add %s skill: %s", name, rel), "content": base64.StdEncoding.EncodeToString(data), "branch": head})
			if err == nil && status/100 != 2 {
				err = fmt.Errorf("GitHub answered %d %s", status, strs.Head(string(body), 200))
			}
		}
		if err != nil {
			return fmt.Sprintf("%s could not be uploaded: %s.", rel, strings.TrimSuffix(err.Error(), ".")), false
		}
	}
	owner, _, _ := strings.Cut(fork.FullName, "/")
	status, body, err = r.githubCall(ctx, http.MethodPost, target+"/pulls", 15*time.Second, map[string]string{
		"title": "Add skill: " + name,
		"body":  fmt.Sprintf("Submitting the `%s` skill from the icebath skills hub.\n\nThe icebath skill scan checked this skill before it was submitted.", name),
		"head":  owner + ":" + head, "base": branch})
	if err != nil {
		return "The pull request could not be made, because GitHub could not be reached: " + strings.TrimSuffix(err.Error(), ".") + ".", false
	}
	if status == 201 {
		var pr struct {
			HTMLURL string `json:"html_url"`
		}
		json.Unmarshal(body, &pr)
		return "✓ Made the pull request " + pr.HTMLURL + ".", true
	}
	return fmt.Sprintf("The pull request could not be made. GitHub answered with status %d: %s", status, strs.Head(strings.ToValidUTF8(string(body), "?"), 200)), false
}
