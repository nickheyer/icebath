package home

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Full control access mask on a file or directory
const fileAllAccess = 0x1F01FF

// ACL revision the added inheriting ACE carries
const aclRevisionDS = 4

// Directory object one elevated grant covered, by path and volume file identity
type grantKey struct {
	path string
	id   fileutil.FileID
}

// Elevation probed once and every directory object this elevated process already granted
type aclState struct {
	once     sync.Once
	elevated bool
	mu       sync.Mutex
	granted  map[grantKey]bool
}

// Grant state of a Windows process, nothing probed or granted yet
func newACLState() *aclState { return &aclState{granted: map[grantKey]bool{}} }

// Grant refused because the target is or became a junction, symlink, other reparse point or hard link
type refusal struct{ msg string }

// Why the grant was refused
func (e *refusal) Error() string { return e.msg }

// Entry a tree grant could not write, with the cause
type grantFailure struct {
	path   string
	reason string
}

// TokenElevation of the token of process
func tokenElevation(process windows.Handle) (bool, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return false, err
	}
	defer token.Close()
	var value, size uint32
	if err := windows.GetTokenInformation(token, windows.TokenElevation, (*byte)(unsafe.Pointer(&value)), uint32(unsafe.Sizeof(value)), &size); err != nil {
		return false, err
	}
	return value != 0, nil
}

// TokenElevation of the process pid, an error when the process or its token cannot be opened
func ProcessElevation(pid int) (bool, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false, fmt.Errorf("open pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	return tokenElevation(h)
}

// Reports whether this process runs with an elevated UAC token, false when the probe fails
func (r *Resolver) Elevated() bool {
	r.acl.once.Do(func() {
		e, err := tokenElevation(windows.CurrentProcess())
		r.acl.elevated = err == nil && e
	})
	return r.acl.elevated
}

// One line repair for a directory this process may not use, an elevated ACL reset
func UnusableDirRemedy(path string) string {
	return fmt.Sprintf(`from an elevated prompt run: icacls "%s" /reset /t`, path)
}

// Reports whether path is a symlink, junction or other reparse point, or cannot be examined at all
func isReparsePoint(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return true
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok && d.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return true
	}
	return fileutil.IsJunction(path)
}

// Handle on path itself, never its link target, with access
func openNoFollow(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// Reports whether path is a file with more than one name, or cannot be examined
func hardLinkedFile(path string) bool {
	h, err := openNoFollow(path, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return true
	}
	return info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 && info.NumberOfLinks > 1
}

// Copy of dacl, or a fresh ACL when nil, with an allow full control ACE for sid appended
func appendAllowACE(dacl *windows.ACL, sid *windows.SID, flags uint8) []byte {
	sidLen := int(windows.GetLengthSid(sid))
	revision := byte(aclRevisionDS)
	var aces []byte
	count := 0
	if dacl != nil {
		header := unsafe.Slice((*byte)(unsafe.Pointer(dacl)), 8)
		raw := unsafe.Slice((*byte)(unsafe.Pointer(dacl)), binary.LittleEndian.Uint16(header[2:]))
		revision = max(raw[0], revision)
		off := 8
		for ; count < int(dacl.AceCount) && off+4 <= len(raw); count++ {
			off = min(off+int(binary.LittleEndian.Uint16(raw[off+2:])), len(raw))
		}
		aces = raw[8:off]
	}
	aceLen := 8 + sidLen
	out := make([]byte, 8+len(aces)+aceLen)
	out[0] = revision
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	binary.LittleEndian.PutUint16(out[4:], uint16(count+1))
	copy(out[8:], aces)
	ace := out[8+len(aces):]
	ace[0], ace[1] = windows.ACCESS_ALLOWED_ACE_TYPE, flags
	binary.LittleEndian.PutUint16(ace[2:], uint16(aceLen))
	binary.LittleEndian.PutUint32(ace[4:], fileAllAccess)
	copy(ace[8:], unsafe.Slice((*byte)(unsafe.Pointer(sid)), sidLen))
	return out
}

// Writes the user full control ACE on the opened object itself, false when already present, a moved name refused
func grantNative(path, expected string) (bool, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	h, err := openNoFollow(path, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return false, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	if expected != "" {
		final, err := finalPath(h)
		if err != nil {
			return false, err
		}
		if !strings.EqualFold(strings.TrimRight(final, `\`), strings.TrimRight(expected, `\`)) {
			return false, &refusal{fmt.Sprintf("%s resolves to %s, not %s", path, final, expected)}
		}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return false, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, &refusal{path + " is a junction, symlink or other reparse point"}
	}
	isDir := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if !isDir && info.NumberOfLinks > 1 {
		return false, &refusal{fmt.Sprintf("%s is a hard link (%d names); its other names may be outside", path, info.NumberOfLinks)}
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	dacl, _, err := sd.DACL()
	if err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return false, err
	}
	inherit := uint8(0)
	if isDir {
		inherit = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	if dacl != nil {
		for i := range uint32(dacl.AceCount) {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return false, err
			}
			flags := ace.Header.AceFlags
			if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(user.User.Sid) &&
				uint32(ace.Mask)&fileAllAccess == fileAllAccess && flags&inherit == inherit && flags&windows.INHERIT_ONLY_ACE == 0 {
				return false, nil
			}
		}
	}
	buf := appendAllowACE(dacl, user.User.Sid, inherit)
	abs, err := sd.ToAbsolute()
	if err != nil {
		return false, err
	}
	if err := abs.SetDACL((*windows.ACL)(unsafe.Pointer(&buf[0])), true, false); err != nil {
		return false, err
	}
	err = windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION, abs)
	runtime.KeepAlive(buf)
	if err != nil {
		return false, err
	}
	return true, nil
}

// Grants the user full control on path itself, true when a written directory still owes its existing children
func grant(path string, isDir bool, expected string) (bool, error) {
	if isReparsePoint(path) || hardLinkedFile(path) {
		return false, &refusal{path + " is a junction, symlink, other reparse point or hard link"}
	}
	written, err := grantNative(path, expected)
	return written && isDir, err
}

// Path and each ancestor strictly below the home of ctx, outermost first, empty off the home
func (r *Resolver) componentsBelowHome(ctx context.Context, path string) ([]string, error) {
	home, err := r.Home(ctx)
	if err != nil {
		return nil, err
	}
	homeAbs, err := filepath.Abs(home)
	if err != nil {
		return nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil
	}
	rel, ok := within(abs, homeAbs)
	if !ok || rel == "." {
		return nil, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	out := make([]string, 0, len(parts))
	for i := 1; i <= len(parts); i++ {
		out = append(out, filepath.Join(append([]string{homeAbs}, parts[:i]...)...))
	}
	return out, nil
}

// Path a handle on path must resolve to when nothing below the home is a link, empty off the home
func (r *Resolver) expectedFinal(ctx context.Context, path string) (string, error) {
	components, err := r.componentsBelowHome(ctx, path)
	if err != nil || len(components) == 0 {
		return "", err
	}
	home := filepath.Dir(components[0])
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, _ := within(abs, home)
	real, err := resolvePath(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, rel), nil
}

// Warns that an elevated grant was refused rather than followed out of the home
func warnRefused(path string, err error) {
	slog.Warn(fmt.Sprintf("Not granting the current user access to %s while elevated (%v): an elevated grant "+
		"never follows a junction or link out of the icebath home. If this is your own link, "+
		"point ICEBATH_HOME (or the link) at the real directory.", path, err))
}

// Gives the user an inheriting full control ACE on path, true when it took, owed children granted when repairing
func (r *Resolver) grantUserAccess(ctx context.Context, path string, repairChildren bool) (bool, error) {
	expected, owed, err := r.grantChecked(ctx, path)
	var refused *refusal
	if errors.As(err, &refused) {
		warnRefused(path, err)
		return false, nil
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not grant the current user access to %s created while elevated (%v); a "+
			"non-elevated icebath may not be able to use it. To repair, from an elevated prompt run: "+
			`icacls "%s" /reset /t`, path, err, path))
		return false, nil
	}
	if repairChildren && owed {
		real := expected
		if real == "" {
			if real, err = resolvePath(path); err != nil {
				return false, err
			}
		}
		var skipped []string
		var entries []treeEntry
		for _, e := range walkWithoutLinks(path, &skipped) {
			if e.path != path {
				entries = append(entries, e)
			}
		}
		if _, _, failed := grantEntries(path, real, entries, skipped); len(failed) > 0 {
			slog.Debug(fmt.Sprintf("Granting the existing entries under %s failed for %d, e.g. %s: %s",
				path, len(failed), failed[0].path, failed[0].reason))
		}
	}
	return true, nil
}

// Refuses any link below the home on the way to path, then grants path itself against its expected final path
func (r *Resolver) grantChecked(ctx context.Context, path string) (string, bool, error) {
	components, err := r.componentsBelowHome(ctx, path)
	if err != nil {
		return "", false, err
	}
	for _, c := range components {
		if isReparsePoint(c) {
			return "", false, &refusal{c + " is a junction, symlink or other reparse point"}
		}
	}
	expected, err := r.expectedFinal(ctx, path)
	if err != nil {
		return "", false, err
	}
	owed, err := grant(path, true, expected)
	return expected, owed, err
}

// One entry of a tree walk
type treeEntry struct {
	path  string
	isDir bool
}

// Root and everything under it in walk order, never entering or listing a link, which lands in skipped instead
func walkWithoutLinks(root string, skipped *[]string) []treeEntry {
	if isReparsePoint(root) {
		*skipped = append(*skipped, root)
		return nil
	}
	var out []treeEntry
	stack := []string{root}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		out = append(out, treeEntry{dir, true})
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		children := make([]string, len(entries))
		for i, e := range entries {
			children[i] = filepath.Join(dir, e.Name())
		}
		slices.SortFunc(children, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
		for _, c := range children {
			switch {
			case isReparsePoint(c):
				*skipped = append(*skipped, c)
			case isDir(c):
				stack = append(stack, c)
			default:
				out = append(out, treeEntry{c, false})
			}
		}
	}
	return out
}

// Grants each walked entry on its own, counting grants and collecting refused and failed entries
func grantEntries(root, realRoot string, entries []treeEntry, skipped []string) (int, []string, []grantFailure) {
	granted := 0
	var failed []grantFailure
	for _, e := range entries {
		rel, _ := within(e.path, root)
		_, err := grant(e.path, e.isDir, filepath.Join(realRoot, rel))
		var refused *refusal
		switch {
		case errors.As(err, &refused):
			skipped = append(skipped, e.path)
		case err != nil:
			failed = append(failed, grantFailure{e.path, err.Error()})
		default:
			granted++
		}
	}
	return granted, skipped, failed
}

// Elevated recursive grant over root and everything under it, never through a link, a pinned root resolved once first
func grantTree(root string, pinRoot bool) (int, []string, []grantFailure, error) {
	var err error
	if pinRoot {
		root, err = resolvePath(root)
	} else {
		root, err = filepath.Abs(root)
	}
	if err != nil {
		return 0, nil, nil, err
	}
	realRoot, err := resolvePath(root)
	if err != nil {
		return 0, nil, nil, err
	}
	var skipped []string
	entries := walkWithoutLinks(root, &skipped)
	granted, skipped, failed := grantEntries(root, realRoot, entries, skipped)
	if len(skipped) > 0 {
		suffix := "ies"
		if len(skipped) == 1 {
			suffix = "y"
		}
		shown := strings.Join(skipped[:min(len(skipped), 5)], ", ")
		if len(skipped) > 5 {
			shown += " ..."
		}
		slog.Warn(fmt.Sprintf("Elevated ACL repair skipped %d junction/symlink/hard-link entr%s under %s (never granted through): %s",
			len(skipped), suffix, root, shown))
	}
	return granted, skipped, failed, nil
}

// Gives the user back the home tree an elevated update may have left Administrators-only, an error while nothing took
func (r *Resolver) RepairHomeACL(ctx context.Context) error {
	if !r.Elevated() {
		return nil
	}
	home, err := r.Home(ctx)
	if err != nil {
		return err
	}
	granted, skipped, failed, err := grantTree(home, true)
	if err != nil {
		return err
	}
	if len(failed) > 0 && granted == 0 {
		return fmt.Errorf("could not restore user access to %s: %s: %s", home, failed[0].path, failed[0].reason)
	}
	if granted == 0 {
		why := "nothing was walkable"
		if len(skipped) > 0 {
			why = skipped[0] + " is a junction, symlink or hard link"
		}
		return fmt.Errorf("could not restore user access to %s: nothing was repaired (%s)", home, why)
	}
	if len(failed) > 0 {
		entries := "ies"
		if len(failed) == 1 {
			entries = "y"
		}
		slog.Warn(fmt.Sprintf("Restoring user access under %s failed for %d entr%s, e.g. %s: %s",
			home, len(failed), entries, failed[0].path, failed[0].reason))
	}
	return nil
}

// Creates the directory, granting every directory it made and an existing leaf once per object when elevated
func (r *Resolver) makeUserDir(ctx context.Context, target string, perm fs.FileMode, parents bool) error {
	if !r.Elevated() {
		return makeDir(target, perm, parents)
	}
	var created []string
	for probe := target; !exists(probe) && filepath.Dir(probe) != probe; probe = filepath.Dir(probe) {
		created = append(created, probe)
	}
	if err := makeDir(target, perm, parents); err != nil {
		return err
	}
	toGrant := []string{target}
	fresh := map[string]bool{}
	if len(created) > 0 {
		toGrant = toGrant[:0]
		for i := len(created) - 1; i >= 0; i-- {
			toGrant = append(toGrant, created[i])
			fresh[created[i]] = true
		}
	}
	components, err := r.componentsBelowHome(ctx, target)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, c := range append(components, toGrant...) {
		if seen[c] {
			continue
		}
		seen[c] = true
		if isReparsePoint(c) {
			warnRefused(c, &refusal{"junction, symlink or other reparse point"})
			return nil
		}
	}
	for _, dir := range toGrant {
		id, err := fileutil.LstatID(dir)
		if err != nil {
			continue
		}
		key := grantKey{dir, id}
		r.acl.mu.Lock()
		done := r.acl.granted[key]
		r.acl.mu.Unlock()
		if !fresh[dir] && done {
			continue
		}
		took, err := r.grantUserAccess(ctx, dir, !fresh[dir])
		if err != nil {
			return err
		}
		if took {
			r.acl.mu.Lock()
			r.acl.granted[key] = true
			r.acl.mu.Unlock()
		}
	}
	return nil
}
