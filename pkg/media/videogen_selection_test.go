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

func TestVideoFalStrictSelectionNousSelectionRoutesManagedEvenWithFalKey(t *testing.T) {
	t.Setenv("FAL_QUEUE_GATEWAY_URL", "https://gateway.nousresearch.com")
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "managed-token")
	t.Setenv("FAL_KEY", "fal-key")
	useNousAccount(t, true)
	gw, err := resolveManagedFalVideoGateway(videoSelectionEnv(t, "nous"))
	if err != nil || gw == nil || gw.GatewayOrigin != "https://gateway.nousresearch.com" || gw.NousUserToken != "managed-token" {
		t.Fatalf("%+v %v", gw, err)
	}
}

func TestVideoFalStrictSelectionFalSelectionMissingKeyErrorsWithoutManagedCall(t *testing.T) {
	t.Setenv("FAL_KEY", "")
	acct := useNousAccount(t, true)
	_, err := resolveManagedFalVideoGateway(videoSelectionEnv(t, "fal"))
	var ve *imageValueError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "video_gen is configured to use fal") || !strings.Contains(err.Error(), "FAL_KEY") {
		t.Fatal(err)
	}
	if acct.calls != 0 {
		t.Fatal("managed gateway probed", acct.calls)
	}
}

func TestVideoFalStrictSelectionNeverConfiguredAutodetectUnchanged(t *testing.T) {
	t.Setenv("FAL_KEY", "fal-key")
	gw, err := resolveManagedFalVideoGateway(videoSelectionEnv(t, ""))
	if err != nil || gw != nil {
		t.Fatal(gw, err)
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
