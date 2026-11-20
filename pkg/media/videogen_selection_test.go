package media

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestConfineSourceImagesVideoGenerateUsesSameChokepoint(t *testing.T) {
	env := unreachableSandbox(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	out := decodeResult(t, handleVideoGenerate(context.Background(), &tools.Call{Env: env, TaskID: "t1",
		Args: map[string]any{"prompt": "animate", "image_url": filepath.Join(t.TempDir(), "nope.png")}}))
	if out["success"] != false || !strings.Contains(out["error"].(string), "Could not read source image") {
		t.Fatal(out)
	}
}

// Environment whose video_gen.provider holds the selection, none when empty
func videoSelectionEnv(t *testing.T, provider string) *tools.Env {
	t.Helper()
	if provider == "" {
		return imageEnv(t, config.Tree{})
	}
	return imageEnv(t, config.Tree{"video_gen": map[string]any{"provider": provider}})
}

func TestVideoFalStrictSelectionFalSelectionMissingKeyErrors(t *testing.T) {
	t.Setenv("FAL_KEY", "")
	err := falVideoSelectionError(videoSelectionEnv(t, "fal"))
	var ve *imageValueError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "video_gen is configured to use fal") || !strings.Contains(err.Error(), "FAL_KEY") {
		t.Fatal(err)
	}
}

func TestVideoFalStrictSelectionNeverConfiguredAutodetectUnchanged(t *testing.T) {
	t.Setenv("FAL_KEY", "fal-key")
	if err := falVideoSelectionError(videoSelectionEnv(t, "")); err != nil {
		t.Fatal(err)
	}
}

func TestVideoGenHappyHorseUsesAlibabaNamespace(t *testing.T) {
	hh := falFamilies["happy-horse"]
	if !strings.HasPrefix(hh.TextEndpoint, "alibaba/happy-horse/") || !strings.HasSuffix(hh.TextEndpoint, "/text-to-video") {
		t.Fatal(hh.TextEndpoint)
	}
	if !strings.HasPrefix(hh.ImageEndpoint, "alibaba/happy-horse/") || !strings.HasSuffix(hh.ImageEndpoint, "/image-to-video") {
		t.Fatal(hh.ImageEndpoint)
	}
}
