package tools

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	filesconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Reasons the file tools hand the approval prompt
const (
	sshConfigRisk    = "writes the SSH client config"
	instructionsRisk = "protected_instruction_file"
)

// Device and descriptor paths whose reads hang or never end
var blockedDevices = strs.Set("/dev/zero", "/dev/random", "/dev/urandom", "/dev/full", "/dev/stdin", "/dev/tty", "/dev/console", "/dev/stdout", "/dev/stderr",
	"/dev/fd/0", "/dev/fd/1", "/dev/fd/2")

// Proc files that leak secrets, arguments, memory layout or memory
var blockedProc = []string{"/fd/0", "/fd/1", "/fd/2", "/environ", "/cmdline", "/maps", "/smaps", "/smaps_rollup", "/numa_maps", "/mem", "/auxv", "/pagemap"}

// Credential folders of a home the file tools never write
var credentialDirs = []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".azure", ".config/gh", ".config/gcloud"}

// Places of icebath's folders the generic file tools never write: the session store, tokens, vaults and sign-ins
var protectedSubpaths = []string{"sessions.db", "sessions.db-wal", "sessions.db-shm", "sessions.db-journal", "state.db", "sessions", "mcp-tokens",
	"pairing", "vault", "browser-profile", "webapp"}

// Folders of icebath's folders the file tools never read, with what a read of the folder and of a file in it is told
var readDenied = []struct{ dir, folder, file string }{
	{"mcp-tokens", "is the icebath MCP token directory and cannot be read directly.", "is an icebath MCP token file and cannot be read directly."},
	{"browser-profile", "is the icebath real-profile browser snapshot directory (copied cookies/logins) and cannot be read directly.",
		"is inside the icebath real-profile browser snapshot (copied cookies/logins) and cannot be read directly."},
	{"vault", "is the icebath credential vault directory and cannot be read directly (secrets are filled server-side by browser_vault).",
		"is inside the icebath credential vault (encrypted secrets + local key) and cannot be read directly (browser_vault resolves them server-side)."},
	{"webapp", "is the icebath Webapp sign-in directory and cannot be read directly.",
		"is an icebath Webapp sign-in file (the browser credential or its sign-in records) and cannot be read directly."},
}

const defenseInDepth = " (Defense-in-depth — not a security boundary; the terminal tool can still bypass.)"

// Path prefixes and exact paths writes to which are refused as system paths
var (
	sensitivePrefixes = []string{"/etc/", "/boot/", "/usr/lib/systemd/", "/private/etc/", "/private/var/db/", "/private/var/root/"}
	sensitiveExact    = strs.Set("/var/run/docker.sock", "/run/docker.sock")
)

// Base names of files that steer agents, in any folder
var instructionNames = strs.Set("agents.md", "claude.md", "soul.md", ".cursorrules")

// Path made absolute and clean as the guards compare it: links resolved on this machine, lexically in a sandbox
func (o *ops) canonical(p string) string {
	if o.host {
		return realpath(o.hostTilde(p))
	}
	if strings.HasPrefix(p, "/") {
		return posixClean(p)
	}
	return p
}

// Folders of icebath the guards cover: the profile's own and the one every profile is kept in
func (o *ops) icebath() []string {
	dirs := []string{realpath(o.cfg.ProfileDir(o.cfg.Profile()))}
	if root := realpath(o.cfg.Dir()); root != dirs[0] {
		dirs = append(dirs, root)
	}
	return dirs
}

// Whether a path is a device or descriptor whose read hangs, or a proc file that leaks a process's state
func blockedDevice(p string) bool {
	if blockedDevices[p] {
		return true
	}
	if !strings.HasPrefix(p, "/proc/") {
		return false
	}
	for _, suffix := range blockedProc {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// Whether the path, any link it goes through, or where it ends up is a blocked device; links followed on this machine only
func (o *ops) device(raw string) bool {
	text := o.hostTilde(raw)
	if !o.host {
		text = raw
	}
	if !filepath.IsAbs(text) && !strings.HasPrefix(text, "/") {
		text = join(o.base, text)
	}
	text = posixClean(text)
	if blockedDevice(text) {
		return true
	}
	if !o.host {
		return false
	}
	seen := map[string]bool{}
	current := text
	for range 20 {
		target, err := os.Readlink(current)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = join(dirname(current), target)
		}
		target = posixClean(target)
		if blockedDevice(target) {
			return true
		}
		if seen[target] {
			break
		}
		seen[target] = true
		current = target
	}
	return blockedDevice(posixClean(realpath(text)))
}

// Kind of special file a host path is whose read would hang, empty for a regular file, a folder or a path that is not there
func specialKind(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	switch mode := st.Mode(); {
	case mode.IsRegular() || mode.IsDir():
		return ""
	case mode&os.ModeNamedPipe != 0:
		return "a FIFO (named pipe)"
	case mode&os.ModeSocket != 0:
		return "a socket"
	case mode&os.ModeCharDevice != 0:
		return "a character device"
	case mode&os.ModeDevice != 0:
		return "a block device"
	}
	return "a special (non-regular) file"
}

// Why a read of the resolved path is refused: icebath's credential stores, token folders and skills hub cache, and project env files
func (o *ops) readBlock(raw, resolved string) string {
	if refusal := ntRefusal(raw, "Read"); refusal != "" {
		return refusal
	}
	p := o.canonical(resolved)
	reason := ""
	dirs := o.icebath()
	for _, dir := range dirs {
		if under(p, filepath.Join(dir, "skills", ".hub")) {
			reason = "is an internal icebath cache file and cannot be read directly to prevent prompt injection. Use the skills_list or skill_view tools instead."
		}
	}
	for _, name := range terminal.CredentialStores {
		for _, dir := range dirs {
			if reason == "" && p == realpath(filepath.Join(dir, filepath.FromSlash(name))) {
				reason = "is an icebath credential store and cannot be read directly. Provider tools consume these credentials through internal channels." +
					defenseInDepth
			}
		}
	}
	for _, denied := range readDenied {
		for _, dir := range dirs {
			blocked := realpath(filepath.Join(dir, denied.dir))
			if reason == "" && under(p, blocked) {
				reason = denied.file + defenseInDepth
				if p == blocked {
					reason = denied.folder + defenseInDepth
				}
			}
		}
	}
	if reason == "" && redact.EnvFiles[strings.ToLower(filepath.Base(p))] {
		reason = "is a secret-bearing environment file and cannot be read to prevent credential leakage. If you need to check the file structure, read " +
			".env.example instead." + defenseInDepth
	}
	if reason == "" {
		return ""
	}
	return "Access denied: " + raw + " " + reason
}

// Whether reads of the resolved path need the passes that mask secret assignments, and whether they need them whatever the hide secrets
// setting says, as icebath's own config files hold its keys
func (o *ops) secretFile(resolved string) (secret, forced bool) {
	p := o.canonical(resolved)
	inside := func(path, root string) bool { return under(o.canonical(path), root) }
	secret = redact.SecretFile(resolved, o.icebath(), inside)
	if !secret || !strings.EqualFold(filepath.Base(p), "config.yaml") {
		return secret, false
	}
	for _, dir := range o.icebath() {
		if under(p, dir) {
			return true, true
		}
	}
	return true, false
}

// Every home the write guards cover: the user's, the home commands get, the profile's home folder, and the home of a ~name the path names
func (o *ops) homes(raw string) []string {
	homes := map[string]bool{}
	if home, err := os.UserHomeDir(); err == nil {
		homes[home] = true
	}
	env := terminal.ProcessEnv()
	for _, home := range []string{terminal.RealHome(o.cfg, env), terminal.SubprocessHome(o.cfg, env)} {
		if home != "" {
			homes[home] = true
		}
	}
	if profile := filepath.Join(o.cfg.ProfileDir(o.cfg.Profile()), "home"); fsx.IsDir(profile) {
		homes[profile] = true
	}
	if !o.host {
		if home := o.sandboxHome(); home != "" {
			homes[home] = true
		}
	}
	if len(raw) > 1 && raw[0] == '~' && raw[1] != '/' && raw[1] != '\\' {
		name, _, _ := strings.Cut(strings.ReplaceAll(raw[1:], `\`, "/"), "/")
		if expanded := o.hostTilde("~" + name); !strings.HasPrefix(expanded, "~") {
			homes[expanded] = true
		}
	}
	var out []string
	for home := range homes {
		out = append(out, realpath(home))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Folders writes are kept to, the write safe root setting, empty for none
func (o *ops) safeRoots() []string {
	var roots []string
	for _, p := range strings.Split(o.cfg.GetString(filesconfig.WriteSafeRoot), string(os.PathListSeparator)) {
		if p != "" {
			roots = append(roots, realpath(o.hostTilde(p)))
		}
	}
	slices.Sort(roots)
	return roots
}

var (
	runtimeOnce sync.Once
	runtimePath string
)

// Path of the icebath binary this process runs from, case folded where the file system ignores case
func runningBinary() string {
	runtimeOnce.Do(func() {
		if exe, err := os.Executable(); err == nil {
			runtimePath = fold(realpath(exe))
		}
	})
	return runtimePath
}

func fold(p string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(p)
	}
	return p
}

// Whether a path is the running icebath binary, its own last link kept when entry is set
func protectedBinary(p string, entry bool) bool {
	binary := runningBinary()
	if binary == "" {
		return false
	}
	if fold(realpath(p)) == binary {
		return true
	}
	if !entry {
		return false
	}
	parent, leaf := splitEntry(p)
	if parent == "" {
		parent = "."
	}
	return fold(filepath.Join(realpath(parent), leaf)) == binary
}

// Kind of refusal of a write to a resolved path: credential, safe_root or none
func (o *ops) denial(homes []string, p string) string {
	for _, home := range homes {
		if p == realpath(filepath.Join(home, ".ssh", "config")) {
			return ""
		}
	}
	for _, home := range homes {
		for _, name := range []string{".ssh/authorized_keys", ".ssh/id_rsa", ".ssh/id_ed25519", ".netrc", ".pgpass", ".npmrc", ".pypirc", ".git-credentials"} {
			if p == realpath(filepath.Join(home, filepath.FromSlash(name))) {
				return "credential"
			}
		}
		for _, dir := range credentialDirs {
			if strings.HasPrefix(p, realpath(filepath.Join(home, filepath.FromSlash(dir)))+string(filepath.Separator)) {
				return "credential"
			}
		}
	}
	for _, system := range []string{"/etc/sudoers", "/etc/passwd", "/etc/shadow"} {
		if p == realpath(system) {
			return "credential"
		}
	}
	for _, prefix := range []string{"/etc/sudoers.d", "/etc/systemd"} {
		if strings.HasPrefix(p, realpath(prefix)+string(filepath.Separator)) {
			return "credential"
		}
	}
	for _, dir := range o.icebath() {
		for _, name := range append([]string{".env", ".anthropic_oauth.json", "auth/google_oauth.json"}, terminal.SecretCaches...) {
			if p == realpath(filepath.Join(dir, filepath.FromSlash(name))) {
				return "credential"
			}
		}
		for _, sub := range protectedSubpaths {
			if under(p, realpath(filepath.Join(dir, sub))) {
				return "credential"
			}
		}
	}
	if roots := o.safeRoots(); len(roots) > 0 && !slices.ContainsFunc(roots, func(root string) bool { return under(p, root) }) {
		return "safe_root"
	}
	return ""
}

// Refusal of a write, delete or move of the path: icebath's binary and credential stores, the system's credential files, the folders
// outside the write safe root, and NT namespace paths; entry keeps a link as the path itself, as a delete or move acts on the link
func (o *ops) denied(p, verb string, entry bool) string {
	if refusal := ntRefusal(p, verb); refusal != "" {
		return refusal
	}
	kind := ""
	if protectedBinary(p, true) || entry && protectedBinary(p, false) {
		kind = "credential"
	}
	homes := o.homes(p)
	if kind == "" {
		kind = o.denial(homes, o.canonical(p))
	}
	if kind == "" && entry {
		parent, leaf := splitEntry(p)
		if parent == "" {
			parent = "."
		}
		kind = o.denial(homes, filepath.Join(o.canonical(parent), leaf))
	}
	switch kind {
	case "":
		return ""
	case "safe_root":
		return verb + " denied: '" + p + "' is outside " + filesconfig.WriteSafeRoot + " (" + strings.Join(o.safeRoots(), string(os.PathListSeparator)) +
			"). Clear the setting or add this path's directory prefix."
	}
	return verb + " denied: '" + p + "' is a protected system/credential file."
}

// Refusal of a write to a system path, to icebath's settings, or to a path that climbs above a sandbox home that could not be found
func (o *ops) sensitive(raw, resolved string) string {
	if refusal := ntRefusal(raw, "Write"); refusal != "" {
		return refusal
	}
	if !o.host && escapesHome(resolved) {
		return "Refusing to write to " + raw + ": it climbs above the sandbox home and the remote home could not be detected, so its target cannot be " +
			"checked. Pass an absolute path."
	}
	candidates := []string{resolved, hostClean(o.hostTilde(raw))}
	for _, c := range candidates {
		if sensitiveExact[c] || slices.ContainsFunc(sensitivePrefixes, func(prefix string) bool { return strings.HasPrefix(c, prefix) }) {
			return "Refusing to write to sensitive system path: " + raw + "\nUse the terminal tool with sudo if you need to modify system files."
		}
	}
	if o.host {
		for _, file := range []string{filepath.Join(o.cfg.ProfileDir(o.cfg.Profile()), "config.yaml"), filepath.Join(o.cfg.Dir(), "config.yaml")} {
			if slices.Contains(candidates, realpath(file)) {
				return "Refusing to write to icebath config file: " + raw + "\nAgent cannot modify security-sensitive configuration. Edit " + file +
					" directly or use /settings instead."
			}
		}
		if reason := o.skillLock(candidates); reason != "" {
			return "Refusing to write to " + raw + ": " + reason
		}
	}
	return ""
}

// Why a write may not land in a skill the user locked, or in the record that keeps the locks; empty when no skill is locked
func (o *ops) skillLock(paths []string) string {
	scope := o.r.skills.Scope(o.cfg, o.call.Env.Cwd)
	locked, err := scope.LockedDirs()
	if err != nil || len(locked) == 0 {
		return ""
	}
	record := realpath(filepath.Join(scope.Dir(), ".usage.json"))
	names := slices.Sorted(maps.Keys(locked))
	for _, p := range paths {
		if p == "" {
			continue
		}
		target := realpath(p)
		if target == record {
			return "skills/.usage.json holds the user's skill locks, so the agent cannot change it while any skill is locked."
		}
		for _, name := range names {
			if under(target, locked[name]) {
				return skills.LockRefusal(name)
			}
		}
	}
	return ""
}

// Label of a file that steers agents the path names, by what was written or where it resolves; empty for any other or one inside icebath
func (o *ops) instructionFile(raw, resolved string) string {
	if !o.cfg.GetBool(filesconfig.ProtectInstructions) {
		return ""
	}
	normalized := hostClean(o.hostTilde(raw))
	target := o.canonical(resolved)
	for _, dir := range o.icebath() {
		if under(target, dir) {
			return ""
		}
	}
	var extra []string
	for _, pattern := range o.cfg.GetStringSlice(filesconfig.ExtraInstructions) {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			extra = append(extra, strings.ToLower(pattern))
		}
	}
	for _, candidate := range []string{normalized, target} {
		base := basename(candidate)
		lower := strings.ToLower(base)
		if instructionNames[lower] {
			return base
		}
		for _, pattern := range extra {
			if ok, err := filepath.Match(pattern, lower); err == nil && ok {
				return base
			}
		}
		parts := strings.Split(strings.TrimRight(strings.ReplaceAll(candidate, `\`, "/"), "/"), "/")
		if len(parts) >= 2 && parts[len(parts)-2] == ".icebath" {
			return candidate
		}
	}
	return ""
}

// Refusal after an approval prompt the user did not answer yes to, naming what was asked
func blocked(what, targets string, err error, after string) string {
	why := "requires approval but the approval request could not be answered (" + err.Error() + ")."
	if refusal, ok := errors.AsType[*tools.Refusal](err); ok {
		switch {
		case refusal.Outcome == "timeout":
			why = "approval prompt timed out without a user response. Silence is not consent."
		case refusal.Outcome == "denied" && refusal.Reason != "":
			why = "was denied by the user (their reason: " + strings.TrimSpace(refusal.Reason) + ")."
		case refusal.Outcome == "denied":
			why = "was denied by the user."
		}
	}
	return "BLOCKED: write to " + what + " (" + targets + ") " + why + after
}

// Asks once before a write that touches a file that steers agents, every such path of the call in the one prompt; empty when approved
func (o *ops) approveInstructions(raws, resolved []string) string {
	var labels []string
	for i, raw := range raws {
		if label := o.instructionFile(raw, resolved[i]); label != "" && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return ""
	}
	targets := strings.Join(labels, ", ")
	description := "The agent wants to write to the instruction file " + targets + ", which steers what agents do. You are always asked " +
		"before such a write, whatever your approval settings."
	if len(labels) > 1 {
		description = "The agent wants to write to the instruction files " + targets + ", which steer what agents do. You are always asked " +
			"before such a write, whatever your approval settings."
	}
	if err := o.call.Env.Gate(o.ctx, tools.Gate{Key: instructionsRisk, Description: description, Target: "Write to " + targets,
		Strict: true}); err != nil {
		return blocked("protected agent-instruction file(s)", targets, err, " The user has NOT consented to this write. Do NOT retry it or attempt the "+
			"same edit via another path (terminal, execute_code, etc.).")
	}
	return ""
}

// Asks before a write to the SSH client config, whose directives can run commands; empty when approved or none is written
func (o *ops) approveSSHConfig(raws, resolved []string) string {
	var targets []string
	for i, raw := range raws {
		p := o.canonical(resolved[i])
		for _, home := range o.homes(raw) {
			if p == realpath(filepath.Join(home, ".ssh", "config")) && !slices.Contains(targets, raw) {
				targets = append(targets, raw)
			}
		}
	}
	if len(targets) == 0 {
		return ""
	}
	joined := strings.Join(targets, ", ")
	if err := o.call.Env.Approve(o.ctx, "Write to "+joined, sshConfigRisk); err != nil {
		return blocked("SSH config file(s)", joined, err, " Do NOT retry it via another path (terminal, execute_code) without the user's explicit consent.")
	}
	return ""
}

const mirrorWarning = "Sandbox-mirror write blocked by soft guard: %s sits under '%s', which is %s Use the host-side tool for authoritative state " +
	"(e.g. ``memory`` for memories), or address the host path directly. To bypass %s with ``cross_profile=True``. (Defense-in-depth — not a " +
	"security boundary; the terminal tool can still bypass.)"

// Soft refusal of a write that lands on a sandbox's copy of icebath's folder, which icebath never reads back; empty for any other path
func (o *ops) mirror(resolved string) string {
	if o.host {
		target := o.canonical(resolved)
		dir, err := terminal.SandboxDir(o.cfg)
		if err != nil {
			return ""
		}
		root := realpath(dir)
		if !under(target, root) {
			return ""
		}
		rel, _ := filepath.Rel(root, target)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i := range parts {
			if i+1 < len(parts) && parts[i] == "home" && parts[i+1] == ".icebath" && i+2 < len(parts) {
				mirrorRoot := filepath.Join(root, filepath.Join(parts[:i+2]...))
				inner := strings.Join(parts[i+2:], "/")
				return fmt.Sprintf(mirrorWarning, target, mirrorRoot, "a per-task mirror created by a non-local terminal backend (docker/daytona/etc.). "+
					"Writes here land on a copy that the host icebath process never reads — the authoritative file is likely '"+inner+"' under the real "+
					"icebath folder.", "this guard after explicit user direction, retry the call")
			}
		}
		return ""
	}
	home := o.lease.Backend.Home
	if home == "" || !o.env.Persistent() {
		return ""
	}
	target := posixClean(resolved)
	mount := posixClean(o.sandboxHomePath(home))
	if !posixUnder(target, mount) || target == mount {
		return ""
	}
	inner := strings.TrimPrefix(target, strings.TrimRight(mount, "/")+"/")
	return fmt.Sprintf(mirrorWarning, target, mount, "the container's bind-mounted home — a per-task mirror that the host icebath process never reads. "+
		"The authoritative file is '"+inner+"' under the real icebath folder.", "after explicit user direction, retry")
}

// Folder the backend keeps icebath's files in, ~ expanded with the sandbox home
func (o *ops) sandboxHomePath(home string) string {
	if strings.HasPrefix(home, "~") {
		if sandbox := o.sandboxHome(); sandbox != "" {
			return sandbox + home[1:]
		}
	}
	return home
}

// Refusal of a text write that would corrupt a document, a SQLite sidecar or an existing binary file
func (o *ops) binaryWrite(raw, resolved string) string {
	extension := ext(raw)
	switch {
	case opaqueDocument(raw):
		return "Refusing to write plain text to binary document '" + raw + "' (" + extension + "). A text write cannot produce a valid document " +
			"container and would corrupt the file (read_file showed you EXTRACTED text, not the real bytes). Use the microsoft-office skill or a " +
			"library like python-docx/openpyxl/python-pptx via the terminal to create or edit this document."
	case sqliteSidecar(raw):
		return "Refusing to write plain text to binary SQLite sidecar '" + raw + "' (" + extension + "). A -wal/-shm/-journal file holds raw " +
			"database pages that SQLite reads on the next open; text there corrupts the database. Use the sqlite3 CLI or a SQLite library via the " +
			"terminal to modify the database instead."
	case !pdf(raw) && !binaryExtension(raw):
		return ""
	}
	exists := false
	if o.host {
		st, err := os.Stat(resolved)
		exists = err == nil && st.Mode().IsRegular()
	} else {
		switch _, status := o.probe(resolved); status {
		case "ok", "bad_size":
			exists = true
		case "missing", "not_regular":
		default:
			return "Refusing to write to '" + raw + "': could not establish whether the target file already exists where this write would execute " +
				"(the terminal environment may be starting, unreachable, or was removed). The file was NOT modified — retry once the environment is " +
				"reachable."
		}
	}
	switch {
	case !exists:
		return ""
	case pdf(raw):
		return "Refusing to overwrite existing PDF '" + raw + "' with plain text. read_file showed you EXTRACTED text, not the real bytes — writing " +
			"text back would destroy the document. Use the pdf skill or a PDF library via the terminal to modify it. (Creating a NEW .pdf file is allowed.)"
	}
	return "Refusing to overwrite existing binary file '" + raw + "' (" + extension + ") with plain text — read_file showed you extracted or mojibake " +
		"text, not the real bytes, and writing text back would destroy the file. Use a binary-aware tool via the terminal to modify it (for SQLite " +
		"databases, the sqlite3 CLI or a SQLite library). (Creating a NEW file with this extension is allowed.)"
}

// Status text read_file gives for an unchanged reread, never real file content
const unchangedMessage = "File unchanged since last read. The content from the earlier read_file result in this conversation is still current — " +
	"refer to that instead of re-reading."

// Whether content is the file tools' own display text rather than bytes meant for a file: the unchanged stub, or numbered read_file rows
func displayText(content string) bool {
	if stripped := strings.TrimSpace(content); stripped != "" && strings.Contains(stripped, unchangedMessage) && runes(stripped) <= 2*runes(unchangedMessage) {
		return true
	}
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(strings.TrimSuffix(line, "\r")) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 {
		return false
	}
	var numbers []int
	for _, line := range lines {
		prefix, _, ok := strings.Cut(strings.TrimLeftFunc(line, unicode.IsSpace), "|")
		if !ok || prefix == "" || strings.ContainsFunc(prefix, func(r rune) bool { return r < '0' || r > '9' }) {
			continue
		}
		if n, err := strconv.Atoi(prefix); err == nil {
			numbers = append(numbers, n)
		}
	}
	if len(numbers) < 2 || float64(len(numbers))/float64(len(lines)) < 0.6 {
		return false
	}
	pairs := 0
	for i := 1; i < len(numbers); i++ {
		if numbers[i] == numbers[i-1]+1 {
			pairs++
		}
	}
	return pairs >= len(numbers)-1
}

// Warning when a relative path resolved outside the folder the session works in
func (o *ops) outside(raw, resolved string) string {
	if o.host && filepath.IsAbs(o.hostTilde(raw)) || !o.host && (strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "~")) {
		return ""
	}
	root := o.base
	if o.host && !under(resolved, realpath(root)) || !o.host && !posixUnder(resolved, posixClean(root)) {
		return "Relative path " + strconv.Quote(raw) + " resolved to " + strconv.Quote(resolved) + ", which is OUTSIDE the active workspace (" +
			strconv.Quote(root) + "). The edit will land in a different directory than the terminal's cwd. If this is not intended (e.g. a " +
			"git-worktree session writing into the main checkout), pass an absolute path under the workspace instead."
	}
	return ""
}
