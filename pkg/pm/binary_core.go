package pm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Project name of the binary core, the name plugin requirements on the core resolve to
const binaryCoreName = "icebath-agent"

// Index the binary's pinned runtime artifacts are published on
const binaryCoreRegistry = "https://pypi.org/simple"

// Whether a tree ships neither pyproject nor uv.lock, a binary install whose core inputs the binary embeds
func binaryCoreTree(root string) bool {
	return runtimeOnlyProject(root) && !isRegularFile(filepath.Join(root, "uv.lock"))
}

// Locked packages of the set this host's target installs
func (s *RuntimeSet) served() ([]RuntimeRequirement, error) {
	reqs, err := s.Requirements()
	if err != nil {
		return nil, err
	}
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	var out []RuntimeRequirement
	for _, r := range reqs {
		if r.ServesTarget(target) {
			out = append(out, r)
		}
	}
	return out, nil
}

// Pyproject and uv.lock of the binary core, the served set pinned exactly with every artifact hash the binary carries
func (s *RuntimeSet) coreProject() ([]byte, []byte, error) {
	reqs, err := s.served()
	if err != nil {
		return nil, nil, err
	}
	name := CanonicalizeName(binaryCoreName)
	var project, lock bytes.Buffer
	fmt.Fprintf(&project, "[project]\nname = %s\nversion = \"0.0.0\"\nrequires-python = \">=3.11\"\ndependencies = [\n", tomliWString(name))
	fmt.Fprintf(&lock, "version = 1\nrevision = 3\nrequires-python = \">=3.11\"\n\n[[package]]\nname = %s\nversion = \"0.0.0\"\nsource = { virtual = \".\" }\ndependencies = [\n", tomliWString(name))
	requiresDist := make([]string, 0, len(reqs))
	for _, r := range reqs {
		spec := r.Name + "==" + r.Version
		entry := "name = " + tomliWString(CanonicalizeName(r.Name))
		if r.Marker != "" {
			spec += " ; " + r.Marker
			entry += ", marker = " + tomliWString(r.Marker)
		}
		fmt.Fprintf(&project, "    %s,\n", tomliWString(spec))
		fmt.Fprintf(&lock, "    { %s },\n", entry)
		requiresDist = append(requiresDist, fmt.Sprintf("{ %s, specifier = %s }", entry, tomliWString("=="+r.Version)))
	}
	project.WriteString("]\n\n[tool.uv]\npackage = false\n")
	fmt.Fprintf(&lock, "]\n\n[package.metadata]\nrequires-dist = [%s]\n", strings.Join(requiresDist, ", "))
	for _, r := range reqs {
		fmt.Fprintf(&lock, "\n[[package]]\nname = %s\nversion = %s\nsource = { registry = %s }\n",
			tomliWString(CanonicalizeName(r.Name)), tomliWString(r.Version), tomliWString(binaryCoreRegistry))
		var wheels []string
		for _, f := range r.Files {
			artifact := fmt.Sprintf("{ url = %s, hash = %s }", tomliWString(f.URL), tomliWString("sha256:"+f.SHA256))
			filename, err := artifactFilename(f.URL)
			if err != nil {
				return nil, nil, err
			}
			if strings.HasSuffix(filename, ".whl") {
				wheels = append(wheels, artifact)
				continue
			}
			fmt.Fprintf(&lock, "sdist = %s\n", artifact)
		}
		lock.WriteString("wheels = [\n")
		for _, wheel := range wheels {
			fmt.Fprintf(&lock, "    %s,\n", wheel)
		}
		lock.WriteString("]\n")
	}
	return project.Bytes(), lock.Bytes(), nil
}

// File name an artifact url publishes, its last path segment unescaped
func artifactFilename(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	return path.Base(parsed.Path), nil
}

// Directory of the install state holding the binary core project
func binaryCoreDir(ctx context.Context, root string) string {
	return filepath.Join(InstallStateDir(ctx, root), "core")
}

// Writes the binary core project into the install state where its bytes differ, returning its directory
func materializeBinaryCore(ctx context.Context, root string) (string, error) {
	project, lock, err := ToolRuntime.coreProject()
	if err != nil {
		return "", err
	}
	dir := binaryCoreDir(ctx, root)
	for name, data := range map[string][]byte{"pyproject.toml": project, "uv.lock": lock} {
		file := filepath.Join(dir, name)
		if current, err := os.ReadFile(file); err == nil && bytes.Equal(current, data) {
			continue
		}
		if err := DurableWriteBytes(file, data); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// Core project a generation resolves against, the checkout itself or the binary core of a binary install
func coreSource(ctx context.Context, root string) (string, error) {
	if !binaryCoreTree(root) {
		return root, nil
	}
	return materializeBinaryCore(ctx, root)
}

// Context whose store holds the toolchain a generation builds on, a binary install's runtime set store when the default lacks it
func coreToolchainContext(ctx context.Context, root string) (context.Context, error) {
	if !binaryCoreTree(root) {
		return ctx, nil
	}
	base, err := BaseVenv(root)
	if err != nil {
		return nil, err
	}
	toolchainCtx, _, err := runtimeToolchainContext(ctx, base)
	return toolchainCtx, err
}

// Sha256 of the core lock a stamp hashes, the binary core lock for a binary install
func coreLockDigest(root string) ([]byte, error) {
	if !binaryCoreTree(root) {
		return uvLockDigest(filepath.Join(root, "uv.lock"))
	}
	_, lock, err := ToolRuntime.coreProject()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(lock)
	return sum[:], nil
}

// Selection a binary install's base environment holds once provisioned with the pinned set, the core alone, empty otherwise
func binaryBaseSelection(ctx context.Context, root string) (map[string]any, error) {
	if !binaryCoreTree(root) {
		return map[string]any{}, nil
	}
	venv, err := SelectedVenv(ctx, root)
	if err != nil {
		return nil, err
	}
	requirements, err := ToolRuntime.requirementsText()
	if err != nil {
		return nil, err
	}
	ready, err := runtimeSetReady(ctx, root, venv, newProvisionGate(venv, requirements))
	if err != nil || !ready {
		return map[string]any{}, err
	}
	stamp, err := newVenv(root).Stamp(ctx, []string{}, []MemberDir{})
	if err != nil {
		return nil, err
	}
	return map[string]any{"stamp": stamp, "extras": []any{}}, nil
}
