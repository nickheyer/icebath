package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Resources resident payload directory every bundled artifact ships
const bundlePayloadDirName = "agent-payload"

// Raised when a tree is not a bundled desktop payload
type notBundledApp struct{ msg string }

func (e *notBundledApp) Error() string { return e.msg }

// App root of a bundled desktop app
type bundleLayout struct {
	AppRoot string
}

// Desktop app holding the payload at projectRoot, a notBundledApp error for any other tree
func resolveBundleLayout(projectRoot string) (*bundleLayout, error) {
	repo, err := pypath.Resolve(projectRoot)
	if err != nil {
		repo = projectRoot
	}
	payload := filepath.Dir(repo)
	if filepath.Base(payload) != bundlePayloadDirName {
		return nil, &notBundledApp{fmt.Sprintf("%s is not inside a %s/ payload — no desktop app contains it", repo, bundlePayloadDirName)}
	}
	resources := filepath.Dir(payload)
	appRoot := filepath.Dir(resources)
	if filepath.Base(resources) == "Resources" && filepath.Base(appRoot) == "Contents" && pypath.PathSuffix(filepath.Dir(appRoot)) == ".app" {
		appRoot = filepath.Dir(appRoot)
	}
	if st, err := os.Stat(appRoot); err != nil || !st.IsDir() {
		return nil, &notBundledApp{fmt.Sprintf("no app directory above the payload at %s", payload)}
	}
	return &bundleLayout{AppRoot: appRoot}, nil
}
