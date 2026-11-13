package backend

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// PayloadCommand is one backend entrypoint of a bundled payload and its path
type PayloadCommand struct {
	Name string
	Path string
}

// PayloadCommands keep the entrypoint order the stamp lists them in
type PayloadCommands []PayloadCommand

// UnmarshalJSON reads the commands object of a stamp in source order
func (c *PayloadCommands) UnmarshalJSON(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("payload commands must be an object")
	}
	out := PayloadCommands{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		var path string
		if err := dec.Decode(&path); err != nil {
			return err
		}
		name := key.(string)
		replaced := false
		for i := range out {
			if out[i].Name == name {
				out[i].Path, replaced = path, true
			}
		}
		if !replaced {
			out = append(out, PayloadCommand{Name: name, Path: path})
		}
	}
	*c = out
	return nil
}

// Path of a named entrypoint, empty when the payload has none
func (c PayloadCommands) Get(name string) string {
	for _, command := range c {
		if command.Name == name {
			return command.Path
		}
	}
	return ""
}

// PayloadRuntime is the launch contract a bundled stamp bakes in, paths relative to the payload root
type PayloadRuntime struct {
	RepoDir  string          `json:"repoDir"`
	ToolsDir string          `json:"toolsDir"`
	Commands PayloadCommands `json:"commands"`
}

// PayloadInfo is the bundled payload with its paths resolved under the app resources
type PayloadInfo struct {
	Root     string
	RepoDir  string
	ToolsDir string
	Commands PayloadCommands
	Shim     string
}

// Bundled payload of an artifact whose stamp says bundled, nil for every other artifact and for dev builds
func BundledPayload(resourcesPath, stampPayload string, rt *PayloadRuntime) *PayloadInfo {
	if stampPayload != "bundled" || rt == nil {
		return nil
	}
	root := filepath.Join(resourcesPath, "agent-payload")
	commands := make(PayloadCommands, len(rt.Commands))
	for i, command := range rt.Commands {
		commands[i] = PayloadCommand{Name: command.Name, Path: filepath.Join(root, command.Path)}
	}
	return &PayloadInfo{Root: root, RepoDir: filepath.Join(root, rt.RepoDir), ToolsDir: filepath.Join(root, rt.ToolsDir), Commands: commands, Shim: commands.Get("hermes")}
}

// Bundled payload an update mechanism runs inside, an error naming the broken install when it has none
func RequireBundledPayload(mechanism, resourcesPath, stampPayload string, rt *PayloadRuntime) (*PayloadInfo, error) {
	payload := BundledPayload(resourcesPath, stampPayload, rt)
	if payload == nil {
		return nil, fmt.Errorf("%s updates require the bundled application payload, which this install has none of", mechanism)
	}
	return payload, nil
}

// Backend of a bundled payload, its commands linked into the user bin on POSIX first
func BundledPayloadBackend(payload *PayloadInfo, args []string, home HomeInputs, log func(string)) ResolvedBackend {
	if runtime.GOOS != "windows" {
		if userHome, err := os.UserHomeDir(); err == nil {
			ProvisionCLILinks(payload.Commands, filepath.Join(userHome, ".local", "bin"), log)
		}
	}
	env := BuildDesktopBackendEnv(home.Env, home.Platform, home)
	env["ICEBATH_RUNTIME_DIR"] = payload.ToolsDir
	return ResolvedBackend{BackendCommand: BackendCommand{Command: payload.Shim, Args: append([]string(nil), args...), Env: env}, Kind: "command", Label: "bundled payload at " + payload.Root}
}

// Install id of the tree at root, the first sixteen hex digits of the sha256 of its canonical path
func InstallIDForRoot(root string, canonicalize func(string) string) string {
	sum := sha256.Sum256([]byte(canonicalize(root)))
	return hex.EncodeToString(sum[:])[:16]
}

// Real path of an install root, the absolute path when it cannot be resolved
func CanonicalizeInstallPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		if abs, err := filepath.Abs(real); err == nil {
			return abs
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
