package tools

import (
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/internal/terminal"
)

const windows = runtime.GOOS == "windows"

// Path made absolute with links resolved along the longest part of it that exists
func realpath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	dir, base := filepath.Split(abs)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" || dir == abs {
		return abs
	}
	return filepath.Join(realpath(dir), base)
}

// Clean POSIX path, keeping exactly two leading slashes as POSIX allows
func posixClean(p string) string {
	if p == "" {
		return "."
	}
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		return "/" + path.Clean(p[1:])
	}
	return path.Clean(p)
}

// Clean path in the style of this machine
func hostClean(p string) string {
	if windows {
		return filepath.Clean(p)
	}
	return posixClean(p)
}

// POSIX join where an absolute part replaces the base
func posixJoin(base, p string) string {
	if strings.HasPrefix(p, "/") || base == "" {
		return p
	}
	if strings.HasSuffix(base, "/") {
		return base + p
	}
	return base + "/" + p
}

// Parent folder in the path style of this machine, keeping a root of slashes whole
func dirname(p string) string {
	if windows {
		return filepath.Dir(p)
	}
	i := strings.LastIndex(p, "/") + 1
	head := p[:i]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// Last element after the final separator of this machine's path style, empty after a trailing one
func basename(p string) string {
	cut := strings.LastIndexByte(p, '/')
	if windows {
		cut = strings.LastIndexAny(p, `/\`)
		if cut < 0 && len(p) >= 2 && p[1] == ':' {
			cut = 1
		}
	}
	return p[cut+1:]
}

// Joins a folder and a name in the path style of this machine
func join(dir, name string) string {
	if windows {
		return filepath.Join(dir, name)
	}
	return posixJoin(dir, name)
}

// Parent and last element of a path, trailing separators and dots dropped, for an entry whose own link must not be followed
func splitEntry(p string) (string, string) {
	volume := filepath.VolumeName(p)
	tail := p[len(volume):]
	separators := "/"
	if windows {
		separators = `\/`
	}
	trimmed := strings.TrimRight(tail, separators)
	if trimmed == "" && tail != "" {
		trimmed = tail[:1]
	}
	parent, leaf := filepath.Split(volume + trimmed)
	if leaf == "." && parent != "" {
		return splitEntry(parent)
	}
	return parent, leaf
}

// Extension of the last element, lower case with its dot, none when the name holds no dot past its leading ones
func ext(p string) string {
	return strings.ToLower(p[len(stem(p)):])
}

// Path without the extension of its last element
func stem(p string) string {
	sep := strings.LastIndex(p, "/")
	if windows {
		sep = max(sep, strings.LastIndex(p, `\`))
	}
	if dot := strings.LastIndex(p, "."); dot > sep && strings.Trim(p[sep+1:dot], ".") != "" {
		return p[:dot]
	}
	return p
}

// Whether a path climbs with a .. element
func traverses(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

// Whether a resolved path is the root or under it, in this machine's path style
func under(p, root string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, strings.TrimRight(root, string(filepath.Separator))+string(filepath.Separator))
}

// Whether a POSIX path is the root or under it
func posixUnder(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimRight(root, "/")+"/")
}

// Whether a raw path is in the Windows NT or device namespace, which skips path normalization
func ntNamespace(p string) bool {
	s := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(s, `\??\`) || strings.HasPrefix(s, `\\.\`) {
		return true
	}
	if rest, ok := strings.CutPrefix(s, `\\?\`); ok {
		upper := strings.ToUpper(rest)
		return strings.HasPrefix(upper, `UNC\`) || strings.HasPrefix(upper, `GLOBALROOT\`)
	}
	return false
}

// Refusal of an NT or device namespace path for the verb, empty for any other path
func ntRefusal(p, verb string) string {
	if !ntNamespace(p) {
		return ""
	}
	return verb + ` denied: '` + p + `' uses a Windows NT/device namespace prefix (\??\, \\.\, \\?\UNC\, or GLOBALROOT). These paths bypass ` +
		"normal path normalization and can trigger outbound SMB authentication (NTLM credential leak) merely by being resolved. Use a normal " +
		"absolute path instead."
}

var msysDrive = regexp.MustCompile(`^/(?:(?:cygdrive|mnt)/)?([a-zA-Z])(/.*)?$`)

var nativeDrive = regexp.MustCompile(`^([A-Za-z]):[\\/]?(.*)$`)

// Git Bash drive path in its native Windows form, unchanged elsewhere
func msysToWindows(p string) string {
	if !windows || p == "" {
		return p
	}
	m := msysDrive.FindStringSubmatch(p)
	if m == nil {
		return p
	}
	tail := strings.ReplaceAll(m[2], "/", `\`)
	if tail == "" {
		tail = `\`
	}
	return strings.ToUpper(m[1]) + ":" + tail
}

// Native Windows drive path in the form Git Bash takes, slashes forward, unchanged elsewhere
func bashPath(p string) string {
	if !windows || p == "" {
		return p
	}
	if m := nativeDrive.FindStringSubmatch(p); m != nil {
		p = "/" + strings.ToLower(m[1]) + "/" + strings.TrimLeft(strings.ReplaceAll(m[2], `\`, "/"), "/")
	}
	return strings.ReplaceAll(p, `\`, "/")
}

// Expands a leading ~ with the home commands of the profile get, else the user's, and ~name with that user's
func (o *ops) hostTilde(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	name, tail := p[1:], ""
	if i := strings.IndexAny(name, `/\`); i >= 0 {
		name, tail = name[:i], name[i:]
	}
	if name != "" {
		if u, err := user.Lookup(name); err == nil && u.HomeDir != "" {
			return strings.TrimRight(u.HomeDir, "/") + tail
		}
		return p
	}
	home := terminal.SubprocessHome(o.cfg, terminal.ProcessEnv())
	if home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return p
	}
	return strings.TrimRight(home, `/\`) + tail
}

// Path as the session's commands see it: on this machine with ~ expanded and links resolved, in a sandbox with a mounted host path moved
// to where the sandbox sees it and ~ the sandbox user's home; a relative path joined onto the folder the commands run in
func (o *ops) resolve(p string) string {
	if o.host {
		text := o.hostTilde(msysToWindows(p))
		if !filepath.IsAbs(text) {
			text = filepath.Join(o.base, text)
		}
		if windows {
			return filepath.Clean(text)
		}
		return realpath(text)
	}
	text := o.sandboxPath(p)
	if !strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "~") {
		text = posixJoin(o.base, text)
	}
	if !strings.HasPrefix(text, "~") {
		return posixClean(text)
	}
	head, tail, _ := strings.Cut(text, "/")
	if tail = posixClean(tail); tail == "." {
		return head
	}
	return head + "/" + tail
}

// Path of the entry itself: its folder resolved, its own last element kept even when it is a link
func (o *ops) resolveEntry(p string) string {
	parent, name := splitEntry(p)
	if name == "" || name == "." || name == ".." || parent == "" && strings.HasPrefix(name, "~") {
		return o.resolve(p)
	}
	if parent == "" {
		parent = "."
	}
	base := o.resolve(parent)
	if !o.host {
		return posixJoin(strings.TrimSuffix(base, "/")+"/", name)
	}
	return filepath.Join(base, name)
}

// Path as the commands of the environment take it: on this machine with ~ expanded, in a sandbox through sandboxPath
func (o *ops) expand(p string) string {
	if o.host {
		return o.hostTilde(p)
	}
	return o.sandboxPath(p)
}

// A sandbox path with a host path under the mounted folder moved to where the sandbox sees it, and ~ expanded inside the sandbox
func (o *ops) sandboxPath(p string) string {
	host, mount := o.env.Mount()
	if translated := terminal.TranslateMounted(p, host, mount); translated != "" {
		return translated
	}
	if !strings.HasPrefix(p, "~") {
		return p
	}
	name := p[1:]
	if i := strings.Index(name, "/"); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		if home := o.sandboxHome(); home != "" {
			return home + p[1:]
		}
		return p
	}
	if !userName.MatchString(name) {
		return p
	}
	result := o.exec("echo ~"+name, "", 0)
	if out := strings.TrimSpace(result.output); result.code == 0 && out != "" && !strings.HasPrefix(out, "~") {
		return out + p[1+len(name):]
	}
	return p
}

var userName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Home of the sandbox user: what its backend declared, else what $HOME says inside it, asked once
func (o *ops) sandboxHome() string {
	if home := o.env.SandboxHome(); home != "" {
		return home
	}
	return o.cache.home(func() string {
		result := o.exec("echo $HOME", "", 0)
		if home := strings.TrimSpace(result.output); result.code == 0 && strings.HasPrefix(home, "/") {
			return home
		}
		return ""
	})
}

// Whether a sandbox path still starting with ~ climbs above the home it is relative to
func escapesHome(resolved string) bool {
	if !strings.HasPrefix(resolved, "~") {
		return false
	}
	_, tail, _ := strings.Cut(resolved, "/")
	return tail == ".." || strings.HasPrefix(tail, "../")
}
