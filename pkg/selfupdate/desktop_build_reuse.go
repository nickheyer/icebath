package selfupdate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Stamp fields a build at HEAD would bake differently, false when that cannot be told
var desktopStampDrift = func(ctx context.Context, projectRoot, baked string, env map[string]string) ([]any, bool) {
	node, ok := sourceNodeBinary(ctx)
	if !ok {
		return nil, false
	}
	result, err := pysubprocess.RunWith(ctx, []string{node, filepath.Join(projectRoot, "scripts", "build", "desktop-reuse.mjs"),
		"--source", projectRoot, "--baked", baked}, pysubprocess.Options{Dir: projectRoot, Env: pm.EnvList(env), Timeout: 60 * time.Second, Check: true})
	if err != nil {
		return nil, false
	}
	lines := pystr.Splitlines(pystr.Strip(pystr.DecodeUTF8Replace(result.Stdout)), false)
	if len(lines) == 0 {
		return nil, false
	}
	var drift any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &drift); err != nil {
		return nil, false
	}
	list, ok := drift.([]any)
	return list, ok
}

// JSON document of a utf-8-sig file, false when unreadable or malformed
func readJSONSig(path string) (any, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, false
	}
	doc, err := pyjson.Loads(text)
	return doc, err == nil
}

// Commit the packaged app was built from when it can stand for HEAD unchanged, empty when it must be built
func packagedDesktopReusable(ctx context.Context, desktopDir, projectRoot string, env map[string]string) string {
	resources := packagedResourcesDir(desktopDir)
	if resources == "" {
		return ""
	}
	bakedPath := filepath.Join(resources, "install-stamp.json")
	baked, ok := readJSONSig(bakedPath)
	if !ok {
		return ""
	}
	prepared, ok := readJSONSig(filepath.Join(desktopDir, "build", "install-stamp.json"))
	if !ok {
		return ""
	}
	obj, isObj := baked.(*pyjson.Object)
	commit := ""
	if isObj {
		commit, _ = obj.GetOr("commit", nil).(string)
	}
	if commit == "" || !pyrepr.Equal(prepared, baked) {
		return ""
	}
	if desktopBuildNeeded(ctx, desktopDir, projectRoot, false) {
		return ""
	}
	if desktopGOOS() == "windows" {
		runner, err := sourceEnsure(ctx, "git", pm.EnsureOptions{BaseEnv: env})
		if err != nil {
			return ""
		}
		env = runner.Env
	}
	if drift, ok := desktopStampDrift(ctx, projectRoot, bakedPath, env); ok && len(drift) == 0 {
		return commit
	}
	return ""
}
