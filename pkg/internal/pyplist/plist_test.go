package pyplist

import (
	"errors"
	"math/big"
	"testing"
)

const xmlDoc = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleShortVersionString</key>
	<string>9.8.7</string>
	<key>n</key>
	<integer>0x1F</integer>
	<key>list</key>
	<array><true/><real>1.5</real><date>2020-01-02T03:04:05Z</date><data>aGk=</data></array>
</dict>
</plist>
`

func classOf(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Class
	}
	return ""
}

func TestLoadXMLValuesAndStr(t *testing.T) {
	v, err := Load([]byte(xmlDoc))
	if err != nil {
		t.Fatal(err)
	}
	d := v.(*Dict)
	if got, _ := d.Get("CFBundleShortVersionString"); got != "9.8.7" {
		t.Fatalf("version %v", got)
	}
	if n, _ := d.Get("n"); n.(*big.Int).Int64() != 31 {
		t.Fatalf("int %v", n)
	}
	list, _ := d.Get("list")
	if got := Str(list); got != "[True, 1.5, datetime.datetime(2020, 1, 2, 3, 4, 5), b'hi']" {
		t.Fatalf("str %q", got)
	}
	if got := Str(list.([]any)[2]); got != "2020-01-02 03:04:05" {
		t.Fatalf("date str %q", got)
	}
}

func TestLoadBinaryDict(t *testing.T) {
	doc := []byte("bplist00")
	doc = append(doc, 0xD1, 0x01, 0x02, 0x51, 'k', 0x55, '9', '.', '8', '.', '7', 0x08, 0x0B, 0x0D)
	trailer := make([]byte, 32)
	trailer[6], trailer[7], trailer[15], trailer[31] = 1, 1, 3, 19
	v, err := Load(append(doc, trailer...))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := v.(*Dict).Get("k"); got != "9.8.7" {
		t.Fatalf("binary value %v", got)
	}
	if _, err := Load(append(doc, make([]byte, 5)...)); classOf(err) != "InvalidFileException" {
		t.Fatalf("truncated trailer %v", err)
	}
}

func TestLoadErrorsNameTheirPythonClass(t *testing.T) {
	cases := map[string]string{
		"invalid plist":                                                   "InvalidFileException",
		"<plist><dict><key>a</key></plist>":                               "ExpatError",
		"<plist><array><key>a</key></array>":                              "ValueError",
		"<plist><dict><key>a</key></dict></plist>":                        "ValueError",
		"<plist><integer>x</integer></plist>":                             "ValueError",
		"<plist><date>2020Z</date></plist>":                               "TypeError",
		"<plist><date>nope</date></plist>":                                "AttributeError",
		"<plist><date>2020-13-01T00:00:00Z</date></plist>":                "ValueError",
		"<plist><key>a</key></plist>":                                     "IndexError",
		"<?xml version=\"1.0\"?><!DOCTYPE x [<!ENTITY a \"b\">]><plist/>": "InvalidFileException",
	}
	for doc, want := range cases {
		if _, err := Load([]byte(doc)); classOf(err) != want {
			t.Errorf("%q: got %v want %s", doc, err, want)
		}
	}
	if v, err := Load([]byte("<plist></plist>")); err != nil || v != nil {
		t.Fatalf("empty plist %v %v", v, err)
	}
}

func TestLoadUTF16XML(t *testing.T) {
	be := append([]byte{0xFE, 0xFF}, xmlBOMs[1].enc(`<?xml version="1.0" encoding="UTF-16"?><plist><string>ok</string></plist>`)...)
	if v, err := Load(be); err != nil || v != "ok" {
		t.Fatalf("utf-16 %v %v", v, err)
	}
}
