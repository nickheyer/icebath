package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Finding is what one doctor check contributed, auto fixable issues, manual only issues and fixes applied
type Finding struct {
	Issues       []string
	ManualIssues []string
	Fixed        int
}

// Merge appends the issues and adds the fix count of another finding
func (f *Finding) Merge(other *Finding) {
	f.Issues = append(f.Issues, other.Issues...)
	f.ManualIssues = append(f.ManualIssues, other.ManualIssues...)
	f.Fixed += other.Fixed
}

// Doctor is one doctor run, the stdout rows print to plus the home, project root and display home checks read
type Doctor struct {
	W           io.Writer
	Home        string
	ProjectRoot string
	DisplayHome string
}

// doctorCheckFunc is one decorated doctor check, it prints its rows and returns what it found
type doctorCheckFunc func(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error)

// doctorRow is the text and optional dim detail of one ok, warn or fail row
type doctorRow struct {
	Text, Detail string
}

// Prints one doctor mark line, the colored glyph, the text and a dim detail when given
func doctorMark(w io.Writer, glyph, col, text, detail string) {
	c := &cliui.Console{Out: w}
	line := "  " + c.Color(glyph, col) + " " + text
	if detail != "" {
		line += " " + c.Color(detail, cliui.Dim)
	}
	fmt.Fprintln(w, line)
}

// Prints a green check doctor line
func doctorCheckOK(w io.Writer, text, detail string) { doctorMark(w, "✓", cliui.Green, text, detail) }

// Prints a yellow warning doctor line
func doctorCheckWarn(w io.Writer, text, detail string) {
	doctorMark(w, "⚠", cliui.Yellow, text, detail)
}

// Prints a red cross doctor line
func doctorCheckFail(w io.Writer, text, detail string) { doctorMark(w, "✗", cliui.Red, text, detail) }

// Prints an indented cyan arrow doctor hint
func doctorCheckInfo(w io.Writer, text string) {
	fmt.Fprintln(w, "    "+(&cliui.Console{Out: w}).Color("→", cliui.Cyan)+" "+text)
}

// Prints the ok row when cond holds, else the warn row or the fail row with fail set, returning cond
func doctorCheckBool(w io.Writer, cond bool, ok, bad doctorRow, fail bool) bool {
	switch {
	case cond:
		doctorCheckOK(w, ok.Text, ok.Detail)
	case fail:
		doctorCheckFail(w, bad.Text, bad.Detail)
	default:
		doctorCheckWarn(w, bad.Text, bad.Detail)
	}
	return cond
}

// Prints a blank line and the bold cyan diamond banner of a doctor section
func doctorSection(w io.Writer, title string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, (&cliui.Console{Out: w}).Color("◆ "+title, cliui.Cyan, cliui.Bold))
}

// Prints a fail row and appends its fix instruction to issues
func doctorFailAndIssue(w io.Writer, text, detail, fix string, issues *[]string) {
	doctorCheckFail(w, text, detail)
	*issues = append(*issues, fix)
}

// Runs fn and reports its error through report with the error substituted for {e}, silent when text is empty
func doctorWarnOnError(w io.Writer, text, detail string, report func(w io.Writer, text, detail string), fn func() error) error {
	err := fn()
	if err == nil || text == "" {
		return nil
	}
	if report == nil {
		report = doctorCheckWarn
	}
	t, ferr := doctorFormatError(text, err)
	if ferr != nil {
		return ferr
	}
	dt, ferr := doctorFormatError(detail, err)
	if ferr != nil {
		return ferr
	}
	report(w, t, dt)
	return nil
}

// Applies str.format with the single keyword e bound to the error text, raising the errors format raises
func doctorFormatError(s string, e error) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '{' && i+1 < len(s) && s[i+1] == '{':
			b.WriteByte('{')
			i++
		case c == '}' && i+1 < len(s) && s[i+1] == '}':
			b.WriteByte('}')
			i++
		case c == '}':
			return "", errors.New("Single '}' encountered in format string")
		case c == '{':
			end := strings.IndexByte(s[i+1:], '}')
			if end < 0 {
				return "", errors.New("expected '}' before end of string")
			}
			field := s[i+1 : i+1+end]
			switch field {
			case "e":
				b.WriteString(e.Error())
			case "":
				return "", errors.New("Replacement index 0 out of range for positional args tuple")
			default:
				return "", fmt.Errorf("'%s'", field)
			}
			i += end + 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// Runs a check body on a fresh finding and propagates its error, the decorator without on_error
func doctorCheck(body func(f *Finding) error) (*Finding, error) {
	f := &Finding{}
	if err := body(f); err != nil {
		return nil, err
	}
	return f, nil
}

// Runs a check body on a fresh finding, warning on its error and keeping the issues recorded before it
func doctorCheckGuarded(w io.Writer, onError, detail string, body func(f *Finding) error) (*Finding, error) {
	f := &Finding{}
	if err := doctorWarnOnError(w, onError, detail, nil, func() error { return body(f) }); err != nil {
		return nil, err
	}
	return f, nil
}

// Prints ok when path exists, creates it with fix counting one fixed, else warns it will be created on first use
func doctorEnsureDir(w io.Writer, f *Finding, shouldFix bool, path, existsMsg, createdMsg, missingMsg string) error {
	if _, err := os.Stat(path); err == nil {
		doctorCheckOK(w, existsMsg, "")
		return nil
	}
	if !shouldFix {
		doctorCheckWarn(w, missingMsg, "(will be created on first use)")
		return nil
	}
	if err := pyfs.Makedirs(path, 0o777, true); err != nil {
		return err
	}
	doctorCheckOK(w, createdMsg, "")
	f.Fixed++
	return nil
}
