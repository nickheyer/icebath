package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/generation"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Most bytes of a source image read
const maxSource = 50 << 20

// Scheme of a URL that names one, a drive letter path naming none
var scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*://`)

// Refusal of a source image, with the class of failure the model reads as its type
type refusal struct {
	kind, text string
}

func (e *refusal) Error() string { return e.text }

// Sources as the service is to read them: where the commands run on this machine, a path of the session's folder made absolute; where they
// run in a sandbox, a path is one the agent saw there, so it is read where it is, the profile's caches here and anything else inside the
// sandbox, and handed over as a data URL. URLs pass as they are
func (r *registrant) confine(ctx context.Context, call *tools.Call, sources []string) ([]string, error) {
	cfg := call.Env.Config
	backend, err := r.terminal.Configured(cfg)
	local := err != nil || backend.Home == "" && !backend.Container && !backend.Remote
	out := make([]string, len(sources))
	for i, source := range sources {
		trimmed := strings.TrimSpace(source)
		lower := strings.ToLower(trimmed)
		if trimmed == "" || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:") {
			out[i] = source
			continue
		}
		if scheme.MatchString(trimmed) && !strings.HasPrefix(lower, "file://") {
			return nil, &refusal{"UnsupportedScheme", "Unrecognized image source scheme. Use an http(s) URL, a local file path, a file:// URI, or a data: URL."}
		}
		p := trimmed
		if strings.HasPrefix(lower, "file://") {
			p = trimmed[len("file://"):]
		}
		if local {
			if p = fsx.ExpandHome(p); !filepath.IsAbs(p) && call.Env.Cwd != "" {
				p = filepath.Join(call.Env.Cwd, p)
			}
			out[i] = p
			continue
		}
		data, err := r.read(ctx, call, backend, p)
		if err != nil {
			return nil, err
		}
		kind := generation.Sniff(data)
		if kind == "" {
			return nil, &refusal{"NotAnImage", "source is not a recognized image"}
		}
		out[i] = generation.DataURL(kind, data)
	}
	return out, nil
}

// Bytes of a path the agent saw in the sandbox: a file of the profile's caches read here, anything else fetched from the sandbox
func (r *registrant) read(ctx context.Context, call *tools.Call, backend terminal.Backend, p string) ([]byte, error) {
	cfg := call.Env.Config
	if host := r.host(call, backend, p); host != "" {
		if refused := generation.Unreadable(cfg, host); refused != "" {
			return nil, &refusal{"SourceUnsafe", refused}
		}
		info, err := os.Stat(host)
		if err == nil && info.Size() > maxSource {
			return nil, &refusal{"SourceTooLarge", "media exceeds size limit"}
		}
		if err == nil && info.Mode().IsRegular() {
			return os.ReadFile(host)
		}
	}
	lease, err := r.terminal.Acquire(ctx, cfg, call.SessionID, call.Env.Cwd)
	if err != nil {
		return nil, &refusal{"SourceNotFound", fmt.Sprintf("'%s' is not reachable inside the sandbox and no active sandbox session is available to read it", p)}
	}
	defer lease.Release()
	mountHost, mount := lease.Env.Mount()
	if translated := terminal.TranslateMounted(p, mountHost, strs.Filled(mount, "/workspace")); translated != "" {
		p = translated
	}
	temp, err := os.CreateTemp("", "icebath-source-*")
	if err != nil {
		return nil, err
	}
	temp.Close()
	defer os.Remove(temp.Name())
	if err := lease.Env.FetchFile(ctx, p, temp.Name(), maxSource); err != nil {
		var fetch *terminal.FetchError
		if errors.As(err, &fetch) && strings.Contains(fetch.Reason, "delivery limit") {
			return nil, &refusal{"SourceTooLarge", "media exceeds size limit"}
		}
		return nil, &refusal{"SourceNotFound", fmt.Sprintf("could not read '%s' inside the sandbox (%s)", p, strs.Head(err.Error(), 200))}
	}
	return os.ReadFile(temp.Name())
}

// Host file a sandbox path of the profile's caches stands for, empty for a path outside them
func (r *registrant) host(call *tools.Call, backend terminal.Backend, p string) string {
	cfg := call.Env.Config
	dir := cfg.ProfileDir(cfg.Profile())
	if backend.Home != "" {
		clean := path.Clean(filepath.ToSlash(p))
		home := strings.TrimRight(backend.Home, "/")
		for _, cache := range terminal.CacheDirs {
			root := home + "/" + cache
			if clean == root || strings.HasPrefix(clean, root+"/") {
				return filepath.Join(dir, filepath.FromSlash(cache), filepath.FromSlash(strings.TrimPrefix(clean[len(root):], "/")))
			}
		}
	}
	if filepath.IsAbs(p) {
		for _, cache := range terminal.CacheDirs {
			if fsx.Within(filepath.Clean(p), filepath.Join(dir, filepath.FromSlash(cache))) {
				return filepath.Clean(p)
			}
		}
	}
	return ""
}
