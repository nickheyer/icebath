package pyurl

import (
	"errors"
	"os"
	"testing"
)

func TestURL2PathnameMatchesCPython(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		url           string
		requireScheme bool
		resolveHost   bool
		windows       bool
		want          string
		wantErr       string
	}{
		{url: "/", want: "/"},
		{url: "/foo", want: "/foo"},
		{url: "///foo", want: "/foo"},
		{url: "////foo", want: "//foo"},
		{url: "//localhost/foo", want: "/foo"},
		{url: "//localhost//foo", want: "//foo"},
		{url: "//localhost///foo", want: "///foo"},
		{url: "//" + hostname + "/foo", want: "/foo"},
		{url: "/foo%20bar", want: "/foo bar"},
		{url: "/caf\u00e9%20x", want: "/caf\u00e9 x"},
		{url: "/foo%ffbar", want: "/foo\xed\xb3\xbfbar"},
		{url: "/foo?bar#baz", want: "/foo"},
		{url: "/foo%3Fbar", want: "/foo?bar"},
		{url: "/a%%41", want: "/a%A"},
		{url: "//foo/bar", wantErr: "<urlopen error file:// scheme is supported only on localhost>"},
		{url: "//:80/foo", wantErr: "<urlopen error file:// scheme is supported only on localhost>"},
		{url: "//127.0.0.1/foo", wantErr: "<urlopen error file:// scheme is supported only on localhost>"},
		{url: "//127.0.0.1/foo", resolveHost: true, want: "/foo"},
		{url: "//[::1/foo", wantErr: "Invalid IPv6 URL"},
		{url: "//\u2100/foo", wantErr: "netloc '\u2100' contains invalid characters under NFKC normalization"},
		{url: "file:///foo", requireScheme: true, want: "/foo"},
		{url: "FILE:///foo", requireScheme: true, want: "/foo"},
		{url: "/foo", requireScheme: true, wantErr: "<urlopen error URL is missing a 'file:' scheme>"},
		{url: "http://x/foo", requireScheme: true, wantErr: "<urlopen error URL is missing a 'file:' scheme>"},
		{url: "/", windows: true, want: `\`},
		{url: "/C:/", windows: true, want: `C:\`},
		{url: "///C|", windows: true, want: "C:"},
		{url: "///C:", windows: true, want: "C:"},
		{url: "///C:/", windows: true, want: `C:\`},
		{url: "/C|//", windows: true, want: `C:\\`},
		{url: "///C|/path", windows: true, want: `C:\path`},
		{url: "///C/test/", windows: true, want: `\C\test\`},
		{url: "////C/test/", windows: true, want: `\\C\test\`},
		{url: "c:/path/to/file", windows: true, want: `c:\path\to\file`},
		{url: "C:/path/to//file", windows: true, want: `C:\path\to\\file`},
		{url: "C|/path/to/file", windows: true, want: `C:\path\to\file`},
		{url: "/C|/path/to/file", windows: true, want: `C:\path\to\file`},
		{url: "///C|/foo/bar/spam.foo", windows: true, want: `C:\foo\bar\spam.foo`},
		{url: "///\u00e8|/", windows: true, want: "\u00e8:\\"},
		{url: "//C:/path/to/file", windows: true, want: `C:\path\to\file`},
		{url: "//server/path/to/file", windows: true, want: `\\server\path\to\file`},
		{url: "////server/path/to/file", windows: true, want: `\\server\path\to\file`},
		{url: "/////server/path/to/file", windows: true, want: `\\server\path\to\file`},
		{url: "//localhost/C:/path/to/file", windows: true, want: `C:\path\to\file`},
		{url: "//localhost/C|/path/to/file", windows: true, want: `C:\path\to\file`},
		{url: "//localhost/path/to/file", windows: true, want: `\path\to\file`},
		{url: "//localhost//server/path/to/file", windows: true, want: `\\server\path\to\file`},
		{url: "//" + hostname + "/C:/x", windows: true, want: `C:\x`},
		{url: "C:/foo%2fbar", windows: true, want: `C:\foo/bar`},
		{url: "//server/share/foo%2fbar", windows: true, want: `\\server\share\foo/bar`},
		{url: "C:/foo%00bar", windows: true, want: "C:\\foo\x00bar"},
		{url: "C:/foo%ED%A0%80", windows: true, want: "C:\\foo\xed\xa0\x80"},
		{url: "C:/foo%ff", windows: true, wantErr: "'utf-8' codec can't decode byte 0xff in position 6: invalid start byte"},
		{url: "file:C:/x", requireScheme: true, windows: true, want: `C:\x`},
		{url: "C:/x", requireScheme: true, windows: true, wantErr: "<urlopen error URL is missing a 'file:' scheme>"},
	}
	for _, tc := range cases {
		got, err := url2pathname(tc.url, tc.requireScheme, tc.resolveHost, tc.windows)
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("url2pathname(%q, windows=%v) error = %v, want %q", tc.url, tc.windows, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("url2pathname(%q, windows=%v) = %q, %v, want %q", tc.url, tc.windows, got, err, tc.want)
		}
	}
	var urlErr *URLError
	if _, err := url2pathname("//foo/x", false, false, false); !errors.As(err, &urlErr) || urlErr.Reason != "file:// scheme is supported only on localhost" {
		t.Errorf("non local authority error = %#v, want URLError", err)
	}
}
