package pypath

import "testing"

func TestSuffixAndWithSuffixMatchPathlib(t *testing.T) {
	for _, c := range []struct{ p, suffix, name string }{
		{"/tmp/out.mp3", ".mp3", "out.mp3"}, {"a/b.tar.gz", ".gz", "b.tar.gz"}, {".bashrc", "", ".bashrc"},
		{"clip.", "", "clip."}, {"dir/", "", "dir"}, {"/", "", ""}, {".", "", ""}, {"noext", "", "noext"},
	} {
		if got := Suffix(c.p); got != c.suffix {
			t.Errorf("Suffix(%q) = %q, want %q", c.p, got, c.suffix)
		}
		if got := Name(c.p); got != c.name {
			t.Errorf("Name(%q) = %q, want %q", c.p, got, c.name)
		}
	}
	for _, c := range []struct{ p, suffix, want string }{
		{"/tmp/out.mp3", ".ogg", "/tmp/out.ogg"}, {"/tmp/out", ".wav", "/tmp/out.wav"},
		{"a/b.tar.gz", ".bz2", "a/b.tar.bz2"}, {"x.mp3", "", "x"}, {".bashrc", ".bak", ".bashrc.bak"},
	} {
		got, err := WithSuffix(c.p, c.suffix)
		if err != nil || got != c.want {
			t.Errorf("WithSuffix(%q, %q) = %q, %v, want %q", c.p, c.suffix, got, err, c.want)
		}
	}
	for _, bad := range []struct{ p, suffix string }{{"x.mp3", "mp3"}, {"x.mp3", "."}, {"x.mp3", "./a"}, {"/", ".mp3"}} {
		if _, err := WithSuffix(bad.p, bad.suffix); err == nil {
			t.Errorf("WithSuffix(%q, %q) accepted", bad.p, bad.suffix)
		}
	}
}
