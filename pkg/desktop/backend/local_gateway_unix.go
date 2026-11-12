//go:build !windows

package backend

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Spawns command directly, or through /bin/sh with the arguments joined when shell is set
func spawnCommand(command string, args []string, shell bool) *exec.Cmd {
	if shell {
		return exec.Command("/bin/sh", "-c", strings.TrimSpace(command+" "+strings.Join(args, " ")))
	}
	return exec.Command(command, args...)
}

// Sends SIGTERM like a default child kill
func terminateProcess(p *os.Process) {
	p.Signal(syscall.SIGTERM)
}

func nodeKind(info fs.FileInfo) string {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return "a symlink"
	case info.IsDir():
		return "a directory"
	case info.Mode().IsRegular():
		return "a regular file"
	case info.Mode()&fs.ModeSocket != 0:
		return "a socket"
	}
	return "another file type"
}

// Checks a control path is this user's and private, true when a home is group writable
func privateNode(file, kind string) (bool, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return false, err
	}
	var valid bool
	switch kind {
	case "directory", "home":
		valid = info.IsDir()
	case "socket":
		valid = info.Mode()&fs.ModeSocket != 0
	case "file":
		valid = info.Mode().IsRegular()
	}
	quoted := shellQuote(file)
	if !valid {
		found := nodeKind(info)
		if kind == "home" {
			return false, fmt.Errorf("Unsafe gateway control path: profile home %s is %s, not a directory: move it aside and recreate the profile", quoted, found)
		}
		return false, fmt.Errorf("Unsafe gateway control path: %s should be a %s but is %s: remove it, then run icebath gateway restart", quoted, kind, found)
	}
	uid := os.Getuid()
	owner := int(info.Sys().(*syscall.Stat_t).Uid)
	if owner != uid {
		return false, fmt.Errorf("Unsafe gateway control path: %s is owned by uid %d, not this user (uid %d): run sudo chown %d %s", quoted, owner, uid, uid, quoted)
	}
	mode := uint32(info.Mode().Perm())
	if kind == "home" && mode&0o002 != 0 {
		return false, fmt.Errorf("Unsafe gateway control path: profile home %s is writable by other users (mode %o): run chmod o-w %s", quoted, mode&0o777, quoted)
	}
	if kind != "home" && mode&0o077 != 0 {
		return false, fmt.Errorf("Unsafe gateway control path: %s is accessible to other users (mode %o): run chmod go-rwx %s", quoted, mode&0o777, quoted)
	}
	return kind == "home" && mode&0o020 != 0, nil
}

// Resolves every symlink of dir one component at a time like the POSIX realpath walk
func posixRealpath(dir string) (string, error) {
	parts := strings.Split(dir, "/")
	rest := make([]string, 0, len(parts))
	for i := len(parts) - 1; i >= 0; i-- {
		rest = append(rest, parts[i])
	}
	resolved := "/"
	if !strings.HasPrefix(dir, "/") {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		resolved = wd
	}
	links := 0
	for len(rest) > 0 {
		name := rest[len(rest)-1]
		rest = rest[:len(rest)-1]
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if i := strings.LastIndex(resolved, "/"); i > 0 {
				resolved = resolved[:i]
			} else {
				resolved = "/"
			}
			continue
		}
		next := "/" + name
		if resolved != "/" {
			next = resolved + "/" + name
		}
		info, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			resolved = next
			continue
		}
		links++
		if links > 40 {
			return "", &fs.PathError{Op: "realpath", Path: dir, Err: syscall.ELOOP}
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(target, "/") {
			resolved = "/"
		}
		tparts := strings.Split(target, "/")
		for i := len(tparts) - 1; i >= 0; i-- {
			rest = append(rest, tparts[i])
		}
	}
	return resolved, nil
}

// Canonical home a ticket names, every symlink resolved
func ReservedHome(dir string) (string, error) { return posixRealpath(dir) }

func resolveControlSocket(home string) (string, error) {
	socketPath := filepath.Join(home, "gateway.sock")
	if _, err := privateNode(socketPath, "socket"); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		pointer := filepath.Join(home, "gateway.sock.path")
		if _, err := privateNode(pointer, "file"); err != nil {
			return "", err
		}
		raw, err := os.ReadFile(pointer)
		if err != nil {
			return "", err
		}
		target := strings.TrimSpace(string(raw))
		sum := sha256.Sum256([]byte(home))
		hash := hex.EncodeToString(sum[:])[:16]
		if !filepath.IsAbs(target) || filepath.Base(filepath.Dir(target)) != "icebath-gw-"+hash || filepath.Base(target) != "control.sock" {
			return "", errors.New("Invalid gateway control pointer")
		}
		if _, err := privateNode(filepath.Dir(target), "directory"); err != nil {
			return "", err
		}
		if _, err := privateNode(target, "socket"); err != nil {
			return "", err
		}
		return target, nil
	}
	return socketPath, nil
}

// Mints a session ticket over the private owner control socket, bound to the endpoint identity
func MintLocalGatewayTicket(endpoint GatewayEndpoint, purpose string) (string, error) {
	if purpose == "" {
		purpose = "interactive"
	}
	home := endpoint.ProfileID
	controlHome := endpoint.controlHome()
	var groupWritable []string
	for _, dir := range uniqueStrings(home, controlHome) {
		canonical, err := ReservedHome(dir)
		if err != nil {
			return "", err
		}
		if canonical != dir {
			return "", errors.New("Noncanonical gateway profile")
		}
		writable, err := privateNode(dir, "home")
		if err != nil {
			return "", err
		}
		if writable {
			groupWritable = append(groupWritable, dir)
		}
	}
	if len(groupWritable) > 0 {
		return mintGroupWritableHome(endpoint, purpose, groupWritable)
	}
	socketPath, err := resolveControlSocket(controlHome)
	if err != nil {
		if isMissingNode(err) {
			return "", errors.New("Gateway ticket control socket missing")
		}
		return "", err
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return "", errors.New("Gateway ticket bootstrap failed")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	params := map[string]any{"profile_id": home, "instance_id": endpoint.InstanceID, "purpose": purpose}
	if scope := TicketScope(purpose); scope != "" {
		params["scope"] = scope
	}
	request, err := json.Marshal(map[string]any{"protocol": 1, "id": 1, "verb": "session-ticket", "params": params})
	if err != nil {
		return "", err
	}
	if _, err := conn.Write(append(request, '\n')); err != nil {
		return "", errors.New("Gateway ticket bootstrap failed")
	}
	line, err := readControlLine(conn)
	if err != nil {
		return "", err
	}
	var reply struct {
		OK       bool `json:"ok"`
		Protocol any  `json:"protocol"`
		ID       any  `json:"id"`
		Result   *struct {
			InstanceID any `json:"instance_id"`
			ProfileID  any `json:"profile_id"`
			Ticket     any `json:"ticket"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		return "", errors.New("Invalid gateway ticket response")
	}
	ticket, isString := "", false
	if reply.Result != nil {
		ticket, isString = reply.Result.Ticket.(string)
	}
	if !reply.OK || reply.Protocol != float64(1) || reply.ID != float64(1) || reply.Result == nil ||
		reply.Result.InstanceID != endpoint.InstanceID || reply.Result.ProfileID != home || !isString {
		return "", errors.New("Invalid gateway ticket response")
	}
	return ticket, nil
}

// First line of a control reply, a clean close is a closed connection and any other fault a failed bootstrap
func readControlLine(conn net.Conn) ([]byte, error) {
	reader := bufio.NewReaderSize(conn, 4096)
	var buf []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > 65536 {
			return nil, errors.New("Gateway ticket bootstrap failed")
		}
		if err == nil {
			return buf[:len(buf)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil, errors.New("Gateway ticket connection closed")
		}
		return nil, errors.New("Gateway ticket bootstrap failed")
	}
}

func uniqueStrings(values ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
