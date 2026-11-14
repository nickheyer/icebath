package app

import (
	"runtime"
	"testing"
)

func TestNativeProjectionHasRequiredUpdateContract(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := storeInitApartmentNative(); err != nil {
		t.Fatal(err)
	}
	defer storeUninitApartmentNative()
	refs := &storeRefs{}
	defer refs.releaseAll()
	context, err := storeDefaultContext(refs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refs.query(context.obj, guidIInitializeWithWindow); err != nil {
		t.Fatal(err)
	}
	if storeUpdateCompleted == storeCaseDownloading {
		t.Fatal("COMPLETED and DOWNLOADING share a value")
	}
	op, err := context.GetAppAndOptionalStorePackageUpdatesAsync()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Status(); err != nil {
		t.Fatal(err)
	}
	if err := op.Cancel(); err != nil {
		t.Fatal(err)
	}
	storePumpWaitingMessages()
}
