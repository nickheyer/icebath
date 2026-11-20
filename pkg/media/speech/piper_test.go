package speech

import (
	"testing"
)

func TestCatalogListsEveryVoiceInOrder(t *testing.T) {
	names := PiperVoices()
	if len(names) < 180 || names[0] >= names[1] {
		t.Fatalf("catalog holds %d voices", len(names))
	}
	for name, pin := range piperArchives {
		if len(pin.sha256) != 64 || pin.size == 0 {
			t.Fatalf("%s is not pinned", name)
		}
	}
	if _, ok := piperArchives["en_US-lessac-medium"]; !ok {
		t.Fatal("the default voice is missing from the catalog")
	}
}
