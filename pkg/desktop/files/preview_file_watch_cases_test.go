package files

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type sent struct {
	event   string
	payload any
}

// fakeOwner stands in for the WebContents that registered a watch
type fakeOwner struct {
	mu        sync.Mutex
	sends     []sent
	destroyed bool
	listeners map[int]func()
	next      int
}

func newFakeOwner() *fakeOwner { return &fakeOwner{listeners: map[int]func(){}} }

func (o *fakeOwner) IsDestroyed() bool { return o.destroyed }

func (o *fakeOwner) Send(event string, payload any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sends = append(o.sends, sent{event, payload})
}

func (o *fakeOwner) OnDestroyed(listener func()) func() {
	id := o.next
	o.next++
	o.listeners[id] = listener
	return func() { delete(o.listeners, id) }
}

func (o *fakeOwner) emitDestroyed() {
	for _, l := range o.listeners {
		l()
	}
}

func (o *fakeOwner) sendCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sends)
}

var previewPayload = PreviewFileChangedPayload{ID: "watch-secondary", Path: "/tmp/plugin.js", URL: "file:///tmp/plugin.js"}

func TestSendPreviewFileChangedToOwnerDeliversToTheOwningWebContentsNotADifferentWindow(t *testing.T) {
	owner, other := newFakeOwner(), newFakeOwner()
	if !SendPreviewFileChangedToOwner(owner, previewPayload, nil) {
		t.Fatal("not delivered")
	}
	if !reflect.DeepEqual(owner.sends, []sent{{PreviewFileChanged, previewPayload}}) || len(other.sends) != 0 {
		t.Fatal(owner.sends, other.sends)
	}
}

func TestSendPreviewFileChangedToOwnerTearsTheWatchDownWhenTheOwnerIsDestroyed(t *testing.T) {
	owner := newFakeOwner()
	owner.destroyed = true
	torn := false
	if SendPreviewFileChangedToOwner(owner, previewPayload, func() { torn = true }) || !torn || len(owner.sends) != 0 {
		t.Fatal(torn, owner.sends)
	}
}

func TestSendPreviewFileChangedToOwnerTearsTheWatchDownWhenTheOwnerIsMissing(t *testing.T) {
	torn := false
	if SendPreviewFileChangedToOwner(nil, previewPayload, func() { torn = true }) || !torn {
		t.Fatal(torn)
	}
}

func TestOnPreviewWatchOwnerDestroyedRunsTheTeardownTheMomentTheOwnerWindowIsDestroyed(t *testing.T) {
	owner := newFakeOwner()
	torn := 0
	OnPreviewWatchOwnerDestroyed(owner, func() { torn++ })
	if torn != 0 {
		t.Fatal(torn)
	}
	owner.emitDestroyed()
	if torn != 1 {
		t.Fatal(torn)
	}
}

func TestOnPreviewWatchOwnerDestroyedAnExplicitWatchCloseDetachesTheDestroyedListener(t *testing.T) {
	owner := newFakeOwner()
	torn := 0
	off := OnPreviewWatchOwnerDestroyed(owner, func() { torn++ })
	off()
	owner.emitDestroyed()
	if torn != 0 {
		t.Fatal(torn)
	}
}

func TestOnPreviewWatchOwnerDestroyedToleratesAMissingOwnerWithoutExploding(t *testing.T) {
	OnPreviewWatchOwnerDestroyed(nil, func() {})()
}

// The registry end to end, a real file edit reaching only the owner and a closed window tearing its watch down
func TestPreviewWatchesDeliverEditsToTheOwnerAndStopWithIt(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plugin.js")
	writeFile(t, file, "v1")
	reg := NewPreviewWatches("linux")
	owner := newFakeOwner()
	res, err := reg.WatchPreviewFile(owner, file)
	if err != nil {
		t.Fatal(err)
	}
	watch := res.(PreviewWatch)
	writeFile(t, file, "v2")
	deadline := time.Now().Add(3 * time.Second)
	for owner.sendCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if owner.sendCount() == 0 {
		t.Fatal("no change delivered")
	}
	owner.mu.Lock()
	got := owner.sends[0].payload.(PreviewFileChangedPayload)
	owner.mu.Unlock()
	if got.ID != watch.ID || got.Path != file || got.URL != pathToFileURL(file) {
		t.Fatal(got)
	}
	owner.emitDestroyed()
	if reg.Stop(watch.ID) {
		t.Fatal("watch outlived its owner")
	}
	missing, err := reg.WatchPreviewFile(owner, filepath.Join(dir, "gone.js"))
	if err != nil || missing.(MissingFileResult).Error != "ENOENT" {
		t.Fatal(missing, err)
	}
	dw, err := reg.WatchDirectory(owner, dir)
	if err != nil || dw.Path != dir {
		t.Fatal(dw, err)
	}
	if _, err := reg.WatchDirectory(owner, file); err == nil || err.Error() != "Not a directory: "+file {
		t.Fatal(err)
	}
	reg.CloseAll()
	if reg.Stop(dw.ID) {
		t.Fatal("close all left a watch")
	}
	os.Remove(file)
}
