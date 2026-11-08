package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyplist"
)

func TestRequestedServiceNameDoesNotBorrowCallersHome(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "named"}[named], func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			requested := filepath.Join(tmp, "custom")
			if named {
				requested = filepath.Join(requested, "profiles", "worker")
			}
			os.MkdirAll(requested, 0o700)
			t.Setenv("ICEBATH_HOME", requested)
			installed, err := ProfileSuffix(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
			if got, err := ServiceSuffix(requested); err != nil || got != installed {
				t.Fatalf("%q %q %v", got, installed, err)
			}
		})
	}
}

func TestMalformedInstalledPlistHasABoundedRefusal(t *testing.T) {
	_, perr := pyplist.Load([]byte(`<?xml version="1.0"?><plist><dict>`))
	var refusal *RuntimeStartError
	if err := verifyServiceBinding(perr); !errors.As(err, &refusal) || refusal.Reason != "service_identity_unverified" {
		t.Fatal(err)
	}
	definition, err := pyplist.Load([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>ai.icebath.gateway</string>
</dict>
</plist>
`))
	if verr := verifyServiceBinding(err); verr != nil {
		t.Fatal(verr)
	}
	d, ok := definition.(*pyplist.Dict)
	if !ok || d.Len() != 1 {
		t.Fatalf("%#v", definition)
	}
	if label, _ := d.Get("Label"); label != "ai.icebath.gateway" {
		t.Fatalf("%#v", definition)
	}
}
