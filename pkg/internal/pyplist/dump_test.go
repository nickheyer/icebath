package pyplist

import (
	"errors"
	"testing"
)

func TestDumpXMLMatchesPlistlib(t *testing.T) {
	got, err := DumpXML(map[string]any{
		"WorkingDirectory":     "/tmp/a&b",
		"Label":                "job",
		"ProgramArguments":     []string{"/usr/bin/env", "<x>\r\ny"},
		"EnvironmentVariables": map[string]string{"B": "2", "A": "1"},
		"Empty":                map[string]string{},
		"None":                 []string{},
		"RunAtLoad":            true,
		"Off":                  false,
		"Count":                3,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Count</key>
	<integer>3</integer>
	<key>Empty</key>
	<dict/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>A</key>
		<string>1</string>
		<key>B</key>
		<string>2</string>
	</dict>
	<key>Label</key>
	<string>job</string>
	<key>None</key>
	<array/>
	<key>Off</key>
	<false/>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/bin/env</string>
		<string>&lt;x&gt;
y</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>WorkingDirectory</key>
	<string>/tmp/a&amp;b</string>
</dict>
</plist>
`
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if back, err := Load(got); err != nil || back == nil {
		t.Fatalf("round trip load failed: %v", err)
	}
}

func TestDumpXMLRefusesControlCharactersAndUnknownTypes(t *testing.T) {
	var perr *Error
	if _, err := DumpXML(map[string]any{"k": "a\x01b"}); !errors.As(err, &perr) || perr.Class != "ValueError" {
		t.Fatalf("control character error = %v", err)
	}
	if _, err := DumpXML(map[string]any{"k": 1.5}); !errors.As(err, &perr) || perr.Class != "TypeError" {
		t.Fatalf("unsupported type error = %v", err)
	}
}
