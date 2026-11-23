//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// One passive PM verdict on startup, the problem text or empty when the install is current, a developer checkout or a build without a release version
func CheckRuntime(ctx context.Context, projectRoot string) (string, error) {
	if updaterecord.BuildVersion() == "" {
		return "", nil
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err == nil {
		if mechanism, _ := pm.ReadInstallStamp(projectRoot).Get("updateMechanism"); mechanism != "self" {
			return "", nil
		}
	}
	problems, err := checkRuntimeActivate(ctx, false)
	if err != nil {
		return "", err
	}
	if len(problems) == 0 {
		return "", nil
	}
	remedy := "Run icebath pm install to fix it."
	if steward := SealedSteward(projectRoot); steward != nil && *steward != "unknown" {
		remedy = "Reinstall icebath with " + *steward + " to fix it."
	} else if steward != nil {
		remedy = "Reinstall icebath the way you installed it to fix it."
	}
	return "Some helper programs icebath uses are missing or out of date (" + strings.Join(problems, "; ") + "). " + remedy, nil
}

// File in the home holding the last install problem reported, so each problem is told once
const runtimeNoteFile = ".install_problem"

// Prints the install problem once per distinct problem, forgetting it once the install is current again
func NoteRuntimeOnce(ctx context.Context, h, projectRoot string, w io.Writer) error {
	problem, err := CheckRuntime(ctx, projectRoot)
	if err != nil {
		return err
	}
	path := filepath.Join(h, runtimeNoteFile)
	if problem == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if told, err := os.ReadFile(path); err == nil && string(told) == problem {
		return nil
	}
	fmt.Fprintln(w, problem)
	return os.WriteFile(path, []byte(problem), 0o600)
}

// Store activation the startup verdict runs, the pm.activate attribute tests replace
var checkRuntimeActivate = pm.Activate
