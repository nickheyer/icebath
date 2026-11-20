package pm

import (
	"io"
	"path/filepath"
)

// Output a relay runtime provisioning reports its progress to
type RuntimeEnvironmentOptions struct {
	Output io.Writer
}

// Relay environment a caller needed that is not provisioned, with what left it so
type RuntimeUnprovisioned struct {
	Venv string
	Err  error
}

func (e *RuntimeUnprovisioned) Error() string {
	return "the runtime environment " + e.Venv + " is not provisioned: " + e.Err.Error()
}

func (e *RuntimeUnprovisioned) Unwrap() error { return e.Err }

// Whether the tree has no project of its own, a binary install whose core the binary embeds
func runtimeOnlyProject(root string) bool {
	return !isRegularFile(filepath.Join(root, "pyproject.toml"))
}
