package gateway

import (
	"bytes"
	"context"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// World whose gateway marks the launch link private
func newPrivateWebappWorld(t *testing.T) *webappWorld {
	w := newWebappWorld(t)
	w.answer("webapp-launch", map[string]any{"ok": true, "result": map[string]any{"url": webappTestLink, "private": true, "expires_in_seconds": int64(60)}})
	return w
}

// Launch pages left in the root home's webapp folder
func (w *webappWorld) pages() []string {
	pages, _ := filepath.Glob(filepath.Join(w.root, "webapp", "launch-*.html"))
	slices.Sort(pages)
	return pages
}

// Runs the webapp command with an explicit profile flag
func (w *webappWorld) runAs(ns map[string]any, explicitProfile string) (int, string) {
	w.t.Helper()
	var out, errOut bytes.Buffer
	code, err := CmdWebapp(context.Background(), w.home, ns, explicitProfile, &out, &errOut)
	if err != nil {
		w.t.Fatalf("CmdWebapp error %v", err)
	}
	return code, out.String()
}

// Local path a file URL names
func pagePath(t *testing.T, raw string) string {
	t.Helper()
	parts, err := url.Parse(raw)
	if err != nil || parts.Scheme != "file" {
		t.Fatalf("url %q", raw)
	}
	return parts.Path
}

// Skips off POSIX hosts
func needsPosixWebapp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
}

func TestAPrivateLinkReachesTheBrowserInAPageOnlyTheUserCanRead(t *testing.T) {
	needsPosixWebapp(t)
	w := newPrivateWebappWorld(t)
	code, out := w.runAs(webappArgs(nil), "")
	if code != 0 || len(w.opened) != 1 || strings.Contains(w.opened[0], "ticket") {
		t.Fatalf("code %d opened %v", code, w.opened)
	}
	page := pagePath(t, w.opened[0])
	if !slices.Equal(w.pages(), []string{page}) || permOf(t, page) != 0o600 || permOf(t, filepath.Dir(page)) != 0o700 {
		t.Fatalf("pages %v page %s", w.pages(), page)
	}
	raw, _ := os.ReadFile(page)
	refresh := regexp.MustCompile(`http-equiv="refresh" content="0;url=([^"]*)"`).FindStringSubmatch(string(raw))
	if refresh == nil || html.UnescapeString(refresh[1]) != webappTestLink {
		t.Fatalf("page %q", raw)
	}
	if !strings.Contains(out, "Opened the Webapp in your browser") || strings.Contains(out, "ticket=") ||
		!strings.Contains(out, "If no browser opened, run `icebath webapp --no-open` for a link.") {
		t.Fatalf("out %q", out)
	}
}

func TestTheNoOpenHintKeepsTheProfileABareRerunWouldNotPick(t *testing.T) {
	needsPosixWebapp(t)
	for _, c := range []struct{ flag, sticky, command string }{
		{"work", "", "icebath -p work webapp --no-open"},
		{"default", "work", "icebath -p default webapp --no-open"},
		{"work", "work", "icebath webapp --no-open"},
	} {
		w := newPrivateWebappWorld(t)
		if c.sticky != "" {
			if err := os.WriteFile(filepath.Join(w.root, "active_profile"), []byte(c.sticky+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		code, out := w.runAs(webappArgs(nil), c.flag)
		if code != 0 || !strings.Contains(out, "If no browser opened, run `"+c.command+"` for a link.") {
			t.Fatalf("%+v code %d out %q", c, code, out)
		}
	}
}

func TestAFolderOtherUsersCouldReadIsClosedBeforeThePageIsWritten(t *testing.T) {
	needsPosixWebapp(t)
	w := newPrivateWebappWorld(t)
	folder := filepath.Join(w.root, "webapp")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(folder, 0o755)
	if code, _ := w.runAs(webappArgs(nil), ""); code != 0 || permOf(t, folder) != 0o700 || len(w.pages()) != 1 {
		t.Fatalf("code %d mode %o pages %v", code, permOf(t, folder), w.pages())
	}
}

func TestARelativeHermesHomeStillReachesTheBrowserInAPage(t *testing.T) {
	needsPosixWebapp(t)
	w := newPrivateWebappWorld(t)
	t.Chdir(w.tmp)
	w.useHome("./root")
	code, out := w.runAs(webappArgs(nil), "")
	if code != 0 || len(w.opened) != 1 {
		t.Fatalf("code %d opened %v", code, w.opened)
	}
	page := pagePath(t, w.opened[0])
	pages := w.pages()
	if !filepath.IsAbs(page) || len(pages) != 1 || strings.Contains(out, "ticket=") {
		t.Fatalf("page %s pages %v out %q", page, pages, out)
	}
	a, _ := filepath.EvalSymlinks(page)
	b, _ := filepath.EvalSymlinks(pages[0])
	if a != b {
		t.Fatalf("page %s pages %v", page, pages)
	}
}

func TestPagesOfEarlierLaunchesAreRemoved(t *testing.T) {
	needsPosixWebapp(t)
	w := newPrivateWebappWorld(t)
	folder := filepath.Join(w.root, "webapp")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "launch-earlier.html"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _ := w.runAs(webappArgs(nil), "")
	pages := w.pages()
	if code != 0 || len(pages) != 1 || filepath.Base(pages[0]) == "launch-earlier.html" {
		t.Fatalf("code %d pages %v", code, pages)
	}
}

func TestWhereNoUserOnlyPageCanCarryItTheLinkIsPrintedAndNoPageIsLeft(t *testing.T) {
	for _, c := range []string{"no-open", "no browser", "wsl", "folder others can read"} {
		w := newPrivateWebappWorld(t)
		switch c {
		case "no browser":
			wl.open = func(target string) bool { w.opened = append(w.opened, target); return false }
		case "wsl":
			wl.isWSL = func() bool { return true }
		case "folder others can read":
			wl.readableByOthers = func(string) bool { return true }
		}
		code, out := w.runAs(webappArgs(map[string]any{"no_open": c == "no-open"}), "")
		for _, u := range w.opened {
			if strings.Contains(u, "ticket") {
				t.Fatalf("%s opened %q", c, u)
			}
		}
		if (c != "no browser" && len(w.opened) != 0) || len(w.pages()) != 0 || code != 0 ||
			!strings.Contains(out, webappTestLink) || !strings.Contains(out, "keep it private") || !strings.Contains(out, "60 seconds") {
			t.Fatalf("%s code %d opened %v pages %v out %q", c, code, w.opened, w.pages(), out)
		}
	}
}

func TestACheckedLinkStillGoesStraightToTheBrowser(t *testing.T) {
	w := newWebappWorld(t)
	if code, _ := w.runAs(webappArgs(nil), ""); code != 0 || !slices.Equal(w.opened, []string{webappTestLink}) || len(w.pages()) != 0 {
		t.Fatalf("code %d opened %v pages %v", code, w.opened, w.pages())
	}
}
