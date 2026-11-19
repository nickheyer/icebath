package tui

import (
	"context"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Module packages the terminal UI may import, the contract, the UI kit, paths, language and the tiers this binary links
var uiImportFence = []string{"pkg/ui/", "pkg/tuigateway", "pkg/i18n", "pkg/home", "pkg/linktier"}

// Packages of the extras tier only its tagged files may import, pets and voxel drawing
var uiExtrasImportFence = []string{"pkg/pet", "pkg/vxl"}

// Whether a file's build constraint negates both lower builds at its top level, the extras tier tag
func extrasTierFile(file *ast.File) bool {
	negated := map[string]bool{}
	var walk func(x constraint.Expr)
	walk = func(x constraint.Expr) {
		switch e := x.(type) {
		case *constraint.AndExpr:
			walk(e.X)
			walk(e.Y)
		case *constraint.NotExpr:
			if t, ok := e.X.(*constraint.TagExpr); ok {
				negated[t.Tag] = true
			}
		}
	}
	for _, group := range file.Comments {
		for _, c := range group.List {
			if expr, err := constraint.Parse(c.Text); err == nil && constraint.IsGoBuild(c.Text) {
				walk(expr)
			}
		}
	}
	return negated["icebath_core"] && negated["icebath_noextras"]
}

// Imports of the package's files, test files included, that reach an engine package past the fence
func importsPastTheFence() ([]string, error) {
	const module = "github.com/nickheyer/icebath/"
	files, err := filepath.Glob("*.go")
	if err != nil {
		return nil, err
	}
	var past []string
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return nil, err
		}
		fence := uiImportFence
		if extrasTierFile(file) {
			fence = append(slices.Clone(uiImportFence), uiExtrasImportFence...)
		}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			rel, inModule := strings.CutPrefix(path, module)
			if !inModule {
				continue
			}
			allowed := false
			for _, prefix := range fence {
				allowed = allowed || rel == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(rel, strings.TrimSuffix(prefix, "/")+"/")
			}
			if !allowed {
				past = append(past, name+" imports "+path)
			}
		}
	}
	return past, nil
}

// Whether a host names this machine
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Refuses the package's imports past the fence, then blocks external network and DNS for every case
func TestMain(m *testing.M) {
	past, err := importsPastTheFence()
	if err != nil || len(past) > 0 {
		fmt.Fprintln(os.Stderr, "the terminal UI knows the backend only through pkg/tuigateway:", err, strings.Join(past, "\n"))
		os.Exit(1)
	}
	transport := http.DefaultTransport.(*http.Transport)
	dial := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || !loopbackHost(host) {
			return nil, fmt.Errorf("network disabled in tests: %s", addr)
		}
		return dial(ctx, network, addr)
	}
	net.DefaultResolver.PreferGo = true
	net.DefaultResolver.Dial = func(_ context.Context, _, address string) (net.Conn, error) {
		return nil, fmt.Errorf("DNS disabled in tests: %s", address)
	}
	os.Exit(m.Run())
}
