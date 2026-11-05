package pm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreFetchResumesInterruptedDownload(t *testing.T) {
	tmp := isolateHome(t)
	runtimeDir := filepath.Join(tmp, "runtime")
	t.Setenv(EnvRuntimeDir, runtimeDir)
	srv := newRangeServer(t)
	srv.mu.Lock()
	srv.chunk = 1 << 16
	srv.mu.Unlock()

	var archiveBytes []byte
	for i := 0; i < 2048; i++ {
		for b := 0; b < 256; b++ {
			archiveBytes = append(archiveBytes, byte(b))
		}
	}
	if len(archiveBytes) >= 1<<20 {
		t.Fatal("single range resume assumes less than 1 MiB")
	}
	name := "faketool-1.0.tar.gz"
	srv.set("/"+name, archiveBytes)
	sha := hexSHA256(archiveBytes)
	url := srv.url("/" + name)

	store := NewStore(filepath.Join(runtimeDir, "store"))
	partials := PartialsRoot()
	key := hexSHA256([]byte(url))

	abortAfter := int64(len(archiveBytes) / 2)
	srv.mu.Lock()
	srv.abortAfter = abortAfter
	srv.mu.Unlock()
	scratch, cleanup, err := store.Scratch()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fetch(url, sha, scratch, nil, nil); err == nil {
		t.Fatal("interrupted fetch succeeded")
	}
	cleanup()

	for _, suffix := range []string{".part", ".ranges"} {
		if st, err := os.Stat(filepath.Join(partials, key+suffix)); err != nil || !st.Mode().IsRegular() {
			t.Fatalf("resume state %s missing: %v", suffix, err)
		}
	}
	first := srv.seen()
	if first[0].Start != 0 {
		t.Fatalf("first request started at %d", first[0].Start)
	}
	if len(first) <= 1 {
		t.Fatal("the persistent outage must exhaust automatic retries")
	}
	for _, r := range first[1:] {
		if r.Start != abortAfter {
			t.Fatalf("retry started at %d, want %d", r.Start, abortAfter)
		}
	}

	srv.mu.Lock()
	srv.abortAfter = -1
	srv.mu.Unlock()
	scratch, cleanup, err = store.Scratch()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got, err := store.Fetch(url, sha, scratch, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, got), archiveBytes) {
		t.Fatal("resumed archive bytes differ")
	}
	if _, err := os.Stat(filepath.Join(partials, key+".part")); !os.IsNotExist(err) {
		t.Fatalf("partial was not consumed: %v", err)
	}
	resumed := srv.seen()[len(first):]
	if len(resumed) != 1 {
		t.Fatalf("expected a single resumed request, saw %v", resumed)
	}
	r := resumed[0]
	if r.Path != "/"+name {
		t.Fatalf("resumed path %q", r.Path)
	}
	if r.Start <= 0 {
		t.Fatalf("resume re-fetched the whole archive: %v", resumed)
	}
	if r.End != int64(len(archiveBytes)-1) {
		t.Fatalf("resume ended at %d", r.End)
	}
	if r.Start >= int64(len(archiveBytes)) {
		t.Fatalf("resume started past the end at %d", r.Start)
	}
}
