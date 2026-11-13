package gateway

import "testing"

func TestExtForMimeWalksOverridesDefaultsAndMimetypesInOrder(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	overrides := map[string]string{"image/heic": ".jpg", "audio/ogg": ".opus"}
	cases := []struct {
		mime                      string
		overrides                 map[string]string
		useDefaults, useMimetypes bool
		fallback, want            string
	}{
		{"IMAGE/HEIC; charset=binary", overrides, true, true, "", ".jpg"},
		{"audio/ogg", overrides, true, true, "", ".opus"},
		{"audio/ogg", nil, true, true, "", ".ogg"},
		{"audio/x-opus+ogg", nil, true, false, ".bin", ".ogg"},
		{"image/bmp", nil, true, false, "", ""},
		{"image/bmp", nil, false, true, "", ".bmp"},
		{"application/x-icebath-unknown", overrides, true, true, ".bin", ".bin"},
		{"image/png", overrides, false, false, ".jpg", ".jpg"},
		{"", overrides, true, true, ".bin", ".bin"},
		{" ; charset=utf-8", nil, true, true, "", ""},
	}
	for _, c := range cases {
		if got := ExtForMime(c.mime, c.overrides, c.useDefaults, c.useMimetypes, c.fallback); got != c.want {
			t.Fatalf("ExtForMime(%q, defaults %v, mimetypes %v, fallback %q) = %q, want %q", c.mime, c.useDefaults, c.useMimetypes, c.fallback, got, c.want)
		}
	}
}
