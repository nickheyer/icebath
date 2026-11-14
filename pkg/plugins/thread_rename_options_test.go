package plugins

import (
	"context"
	"testing"
)

// Discord adapter double recording the guard options of every rename
type guardRecordingAdapter struct {
	*paAdapter
	opts []ThreadRenameOptions
}

func (a *guardRecordingAdapter) RenameThread(_ context.Context, thread, name string, opts ThreadRenameOptions) (bool, error) {
	a.renamed = append(a.renamed, thread+"/"+name)
	a.opts = append(a.opts, opts)
	return true, nil
}

// platform_actions.py _discord_set_thread_title calls rename_thread(thread_id, title) with no guard keyword
func TestDiscordSetThreadTitleRenamesWithoutGuard(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	pa := &PlatformActions{pluginID: "p"}
	dc := &guardRecordingAdapter{paAdapter: &paAdapter{platform: "discord", connected: true}}
	ctx := paSetup(t, paGranted, paRunner{map[string]any{"discord": dc}})
	if r := pa.SetThreadTitle(ctx, "discord", "123", "999", "Renamed"); r["ok"] != true || dc.renamed[0] != "999/Renamed" {
		t.Fatalf("discord rename %v %v", r, dc.renamed)
	}
	if len(dc.opts) != 1 || dc.opts[0].OnlyIfCurrentName != nil || dc.opts[0].PreferConnectorCreated || dc.opts[0].ParentChatID != nil {
		t.Fatalf("rename options %#v", dc.opts)
	}
}
