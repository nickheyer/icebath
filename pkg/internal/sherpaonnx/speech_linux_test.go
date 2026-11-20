//go:build linux && !386 && !arm && !faketime

package sherpaonnx

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/fetch"
)

func TestEveryPlatformArchiveIsPinned(t *testing.T) {
	for gpu, table := range map[bool]map[string]fetch.Asset{false: libraryArchives, true: gpuArchives} {
		for target := range table {
			goos, goarch, _ := strings.Cut(target, "/")
			asset, ok := libraryArchive(goos, goarch, gpu)
			if !ok || len(asset.SHA256) != 64 || asset.Size == 0 || !strings.HasPrefix(asset.URL, "https://github.com/k2-fsa/sherpa-onnx/releases/download/v"+Version+"/") {
				t.Fatalf("%s is not pinned: %+v", target, asset)
			}
		}
	}
	if _, ok := libraryArchive("plan9", "amd64", false); ok {
		t.Fatal("an unpublished platform has an archive")
	}
	if _, ok := libraryArchive("darwin", "arm64", true); ok {
		t.Fatal("a platform without a GPU build has a GPU archive")
	}
}
