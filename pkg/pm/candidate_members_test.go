//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCandidateMembersProposesTheSelectionOfOneHome(t *testing.T) {
	root, _ := wsHomes(t)
	for rel, body := range map[string]string{
		"modern/pyproject.toml": "[project]\ndependencies = [\"requests>=2\"]\n",
		"newbie/pyproject.toml": "[project]\ndependencies = [\"httpx>=0.27\"]\n",
		"plain/plugin.yaml":     "name: plain\n",
	} {
		peWrite(t, filepath.Join(root, "plugins", rel), body)
	}
	extra := filepath.Join(t.TempDir(), "extra")
	peWrite(t, filepath.Join(extra, "pyproject.toml"), "[project]\ndependencies = [\"rich>=13\"]\n")
	wsWriteConfig(t, root, "modern", "plain")
	plugins := filepath.Join(root, "plugins")
	sources := func(extraDirs []string, proposedHome string, enabled, disabled []string) ([]string, error) {
		members, err := candidateMembersFor(context.Background(), extraDirs, &EnabledPluginsOptions{ProposedHome: proposedHome, Enabled: enabled, Disabled: disabled})
		out := make([]string, len(members))
		for i, member := range members {
			out[i] = member.Source
		}
		return out, err
	}
	got, err := sources([]string{extra}, root, []string{"modern", "newbie"}, nil)
	if want := []string{filepath.Join(plugins, "modern"), filepath.Join(plugins, "newbie"), extra}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("proposed members %v %v", got, err)
	}
	got, err = sources(nil, "", []string{"newbie"}, nil)
	if want := []string{filepath.Join(plugins, "modern")}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded members %v %v", got, err)
	}
	got, err = sources(nil, root, []string{"modern", "newbie"}, []string{"modern"})
	if want := []string{filepath.Join(plugins, "newbie")}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled members %v %v", got, err)
	}
}
