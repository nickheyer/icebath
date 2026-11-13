package app

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Hidden first argument that runs the Microsoft Store helper the desktop spawns
const WindowsStoreUpdateModuleEntry = "__icebath_windows_store_update__"

const windowsStoreUpdateDoc = "Run a Store check, download, or install with the packaged icebath binary.\n\n" +
	"The desktop keeps the backend alive during download, then stops it before\n" +
	"requesting installation. Each invocation reports one final JSON result.\n" +
	"Exit 0 means success, 2 means a check found updates, and 1 means failure.\n\n" +
	"Run with the bundled icebath binary: ``icebath " + WindowsStoreUpdateModuleEntry + "``.\n"

// Seconds a Store request may keep its consent and progress UI open
const storeRequestTimeoutS = 1800

// Store package update state of a download or install request
type storeUpdateState int32

const storeUpdateCompleted storeUpdateState = 3

// Member names of StorePackageUpdateState as the Windows Runtime projection spells them
var storeUpdateStateNames = []string{"PENDING", "DOWNLOADING", "DEPLOYING", "COMPLETED", "CANCELED", "OTHER_ERROR",
	"ERROR_LOW_BATTERY", "ERROR_WI_FI_RECOMMENDED", "ERROR_WI_FI_REQUIRED"}

// State of a raw enum value, a value outside the enum failing the way the projection's IntEnum does
func storeUpdateStateOf(value int32) (storeUpdateState, error) {
	if value < 0 || int(value) >= len(storeUpdateStateNames) {
		return 0, fmt.Errorf("%d is not a valid StorePackageUpdateState", value)
	}
	return storeUpdateState(value), nil
}

func (s storeUpdateState) name() string { return storeUpdateStateNames[s] }

// Asynchronous Store operation status the wait polls until it leaves started
type storeAsyncStatus int32

const storeAsyncStarted storeAsyncStatus = 0

// Running Store operation the wait polls and cancels
type storeOperation interface {
	Status() (storeAsyncStatus, error)
	Cancel() error
}

// Store update query whose results are the available package updates
type storeUpdatesOperation interface {
	storeOperation
	GetResults() (storeUpdateList, error)
}

// Store download or install request whose result carries the overall update state
type storeOutcomeOperation interface {
	storeOperation
	GetResults() (storeUpdateOutcome, error)
}

// Package updates a Store query found
type storeUpdateList interface {
	Len() (int, error)
	FullName(i int) (string, error)
}

// Result of a finished download or install request
type storeUpdateOutcome interface {
	OverallState() (storeUpdateState, error)
}

// StoreContext calls the update flow makes, the Windows Runtime StoreContext in production
type storeContext interface {
	GetAppAndOptionalStorePackageUpdatesAsync() (storeUpdatesOperation, error)
	RequestDownloadStorePackageUpdatesAsync(updates storeUpdateList) (storeOutcomeOperation, error)
	RequestDownloadAndInstallStorePackageUpdatesAsync(updates storeUpdateList) (storeOutcomeOperation, error)
}

// Queries the updates and runs the requested operation on that same update sequence, requiring the completed state
func storePerform(context storeContext, mode string, wait func(storeOperation) error, completedState storeUpdateState) (*pyjson.Object, error) {
	query, err := context.GetAppAndOptionalStorePackageUpdatesAsync()
	if err != nil {
		return nil, err
	}
	if err := wait(query); err != nil {
		return nil, err
	}
	updates, err := query.GetResults()
	if err != nil {
		return nil, err
	}
	count, err := updates.Len()
	if err != nil {
		return nil, err
	}
	packages := make([]any, 0, count)
	for i := range count {
		name, err := updates.FullName(i)
		if err != nil {
			return nil, err
		}
		packages = append(packages, name)
	}
	result := pyjson.ObjectOf("available", count > 0, "packages", packages, "ok", true)
	if mode == "check" || count == 0 {
		return result, nil
	}
	var request func(storeUpdateList) (storeOutcomeOperation, error)
	switch mode {
	case "download":
		request = context.RequestDownloadStorePackageUpdatesAsync
	case "install":
		request = context.RequestDownloadAndInstallStorePackageUpdatesAsync
	default:
		return nil, errors.New(pyrepr.StrRepr(mode))
	}
	operation, err := request(updates)
	if err != nil {
		return nil, err
	}
	if err := wait(operation); err != nil {
		return nil, err
	}
	outcome, err := operation.GetResults()
	if err != nil {
		return nil, err
	}
	state, err := outcome.OverallState()
	if err != nil {
		return nil, err
	}
	if state != completedState {
		result.Set("ok", false)
		result.Set("error", fmt.Sprintf("Microsoft Store %s: %s", mode, state.name()))
	}
	return result, nil
}

// Joins the Windows Runtime apartment, replaced only where a case removes the projection
var storeInitApartment = storeInitApartmentNative

// Runs one Store check, download or install and prints its JSON result, exiting 2 when a check found updates
func WindowsStoreUpdateMain(argv []string, stdout, stderr io.Writer) int {
	mode, hwnd, code, done := parseWindowsStoreUpdateArgs(argv, stdout, stderr)
	if done {
		return code
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := storeInitApartment(); err != nil {
		if err := storePrintResult(stdout, pyjson.ObjectOf("available", nil, "ok", false, "error", err.Error())); err != nil {
			fmt.Fprintln(stderr, err)
		}
		return 1
	}
	result := storeExecuteGuarded(mode, hwnd)
	storeUninitApartmentNative()
	if err := storePrintResult(stdout, result); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if ok, _ := result.Get("ok"); ok != true {
		return 1
	}
	if available, _ := result.Get("available"); mode == "check" && available == true {
		return 2
	}
	return 0
}

// Store request result, any failure or panic reported as a failed request of unknown availability
func storeExecuteGuarded(mode string, hwnd *big.Int) (result *pyjson.Object) {
	failed := func(text string) *pyjson.Object {
		return pyjson.ObjectOf("available", nil, "ok", false, "error", "Microsoft Store request failed: "+text)
	}
	defer func() {
		if r := recover(); r != nil {
			result = failed(fmt.Sprint(r))
		}
	}()
	got, err := storeExecuteNative(mode, hwnd)
	if err != nil {
		return failed(err.Error())
	}
	return got
}

// Writes the result as one JSON line the desktop parses
func storePrintResult(stdout io.Writer, result *pyjson.Object) error {
	text, err := pyjson.Dumps(result)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, text)
	return err
}

// Runs the Store update entry and exits when this process was started as one
func DispatchWindowsStoreUpdateEntry() {
	if len(os.Args) > 1 && os.Args[1] == WindowsStoreUpdateModuleEntry {
		os.Exit(WindowsStoreUpdateMain(os.Args[2:], os.Stdout, os.Stderr))
	}
}

// Option strings of the entry parser in the order argparse registers them
var storeUpdateOptions = []string{"-h", "--help", "--mode", "--hwnd"}

// Arguments argparse 3.14 reads as negative numbers, positionals for a parser without numeric options
var storeUpdateNegativeNumber = regexp.MustCompile(`^-\.?\p{Nd}`)

// Digit count past which int refuses a decimal string, the default sys int_max_str_digits
const storeUpdateMaxStrDigits = 4300

// One reading of an option looking argument, an empty option string standing for an unknown option
type storeUpdateTuple struct {
	option   string
	sep      string
	explicit *string
}

// Argparse 3.14 of the Store update entry, the mode, the window handle and an exit code when it ends the run
func parseWindowsStoreUpdateArgs(argv []string, stdout, stderr io.Writer) (string, *big.Int, int, bool) {
	cols, _ := pyshutil.GetTerminalSize(80, 24)
	width := cols - 2
	theme := pm.ArgparseThemeFor(stdout)
	prog := "icebath " + WindowsStoreUpdateModuleEntry
	fail := func(msg string) (string, *big.Int, int, bool) {
		fmt.Fprintf(stderr, "%s%s: error: %s\n", storeUpdateUsage(prog, width, theme), prog, msg)
		return "", nil, 2, true
	}
	readings := make([][]storeUpdateTuple, len(argv))
	for i, arg := range argv {
		if arg == "--" {
			break
		}
		readings[i] = storeUpdateParseOptional(arg)
	}
	mode := "check"
	var hwnd *big.Int
	var extras []string
	for i := 0; i < len(argv); i++ {
		tuples := readings[i]
		if tuples == nil {
			extras = append(extras, argv[i])
			continue
		}
		if len(tuples) > 1 {
			names := make([]string, len(tuples))
			for j, t := range tuples {
				names[j] = t.option
			}
			return fail(fmt.Sprintf("ambiguous option: %s could match %s", argv[i], strings.Join(names, ", ")))
		}
		t := tuples[0]
		if t.option == "" {
			extras = append(extras, argv[i])
			continue
		}
		if t.option == "-h" || t.option == "--help" {
			if msg := storeUpdateHelpChain(t); msg != "" {
				return fail(msg)
			}
			fmt.Fprint(stdout, storeUpdateHelp(prog, width, theme))
			return "", nil, 0, true
		}
		var value string
		if t.explicit != nil {
			value = *t.explicit
		} else {
			if i+1 >= len(argv) || readings[i+1] != nil || argv[i+1] == "--" {
				return fail("argument " + t.option + ": expected one argument")
			}
			i++
			value = argv[i]
		}
		if t.option == "--mode" {
			if value != "check" && value != "download" && value != "install" {
				return fail("argument --mode: invalid choice: " + pyrepr.StrRepr(value) + " (choose from 'check', 'download', 'install')")
			}
			mode = value
			continue
		}
		n, ok := pyint.Parse(value)
		digits := 0
		for _, r := range value {
			if unicode.Is(unicode.Nd, r) {
				digits++
			}
		}
		if !ok || digits > storeUpdateMaxStrDigits {
			return fail("argument --hwnd: invalid int value: " + pyrepr.StrRepr(value))
		}
		hwnd = n
	}
	if len(extras) > 0 {
		return fail("unrecognized arguments: " + strings.Join(extras, " "))
	}
	return mode, hwnd, 0, false
}

// Error an explicit argument after -h or --help raises, empty when the chained short options reach the help action
func storeUpdateHelpChain(t storeUpdateTuple) string {
	option, sep, explicit := t.option, t.sep, t.explicit
	for explicit != nil {
		arg := *explicit
		if option[1] == '-' || arg == "" || sep != "" || arg[0] == '-' {
			return "argument -h/--help: ignored explicit argument " + pyrepr.StrRepr(arg)
		}
		_, size := utf8.DecodeRuneInString(arg)
		if arg[:size] != "h" {
			return ""
		}
		rest := arg[size:]
		switch {
		case rest == "":
			explicit = nil
		case rest[0] == '=':
			attached := rest[1:]
			sep, explicit = "=", &attached
		default:
			sep, explicit = "", &rest
		}
	}
	return ""
}

// Readings of an argument as an option the way argparse 3.14 parses an optional, nil for a positional
func storeUpdateParseOptional(arg string) []storeUpdateTuple {
	if arg == "" || arg[0] != '-' {
		return nil
	}
	if slices.Contains(storeUpdateOptions, arg) {
		return []storeUpdateTuple{{option: arg}}
	}
	if len(arg) == 1 {
		return nil
	}
	if name, value, ok := strings.Cut(arg, "="); ok && slices.Contains(storeUpdateOptions, name) {
		return []storeUpdateTuple{{option: name, sep: "=", explicit: &value}}
	}
	if tuples := storeUpdateOptionTuples(arg); len(tuples) > 0 {
		return tuples
	}
	if storeUpdateNegativeNumber.MatchString(arg) || strings.Contains(arg, " ") {
		return nil
	}
	return []storeUpdateTuple{{}}
}

// Every option an abbreviated option string or a short option with its value attached can name
func storeUpdateOptionTuples(arg string) []storeUpdateTuple {
	prefix, value, hasSep := strings.Cut(arg, "=")
	sep, explicit := "", (*string)(nil)
	if hasSep {
		sep, explicit = "=", &value
	}
	var out []storeUpdateTuple
	for _, option := range storeUpdateOptions {
		switch {
		case arg[1] != '-' && len(option) == 2 && strings.HasPrefix(arg, option):
			short := arg[2:]
			out = append(out, storeUpdateTuple{option: option, explicit: &short})
		case strings.HasPrefix(option, prefix):
			out = append(out, storeUpdateTuple{option: option, sep: sep, explicit: explicit})
		}
	}
	return out
}

// Usage block in the argparse theme, wrapped the way argparse 3.14 wraps a usage wider than the text
func storeUpdateUsage(prog string, width int, t pm.ArgparseColors) string {
	const prefix = "usage: "
	type part struct{ text, plain string }
	progPart := part{t.Prog + prog + t.Reset, prog}
	parts := []part{
		{"[" + t.SummaryShort + "-h" + t.Reset + "]", "[-h]"},
		{"[" + t.SummaryLong + "--mode " + t.SummaryLabel + "{check,download,install}" + t.Reset + "]", "[--mode {check,download,install}]"},
		{"[" + t.SummaryLong + "--hwnd " + t.SummaryLabel + "HWND" + t.Reset + "]", "[--hwnd HWND]"},
	}
	head := t.Usage + prefix + t.Reset
	texts := []string{progPart.text}
	plainLen := len(prog)
	for _, p := range parts {
		texts = append(texts, p.text)
		plainLen += 1 + len(p.plain)
	}
	if len(prefix)+plainLen <= width {
		return head + strings.Join(texts, " ") + "\n"
	}
	lines := func(parts []part, indent string, prefixed bool) []string {
		var out, line []string
		lineLen := len(indent) - 1
		if prefixed {
			lineLen = len(prefix) - 1
		}
		for _, p := range parts {
			if lineLen+1+len(p.plain) > width && len(line) > 0 {
				out = append(out, indent+strings.Join(line, " "))
				line, lineLen = nil, len(indent)-1
			}
			line = append(line, p.text)
			lineLen += len(p.plain) + 1
		}
		if len(line) > 0 {
			out = append(out, indent+strings.Join(line, " "))
		}
		if prefixed {
			out[0] = out[0][len(indent):]
		}
		return out
	}
	var wrapped []string
	if float64(len(prefix)+len(prog)) <= 0.75*float64(width) {
		wrapped = lines(append([]part{progPart}, parts...), strings.Repeat(" ", len(prefix)+len(prog)+1), true)
	} else {
		wrapped = append([]string{progPart.text}, lines(parts, strings.Repeat(" ", len(prefix)), false)...)
	}
	return head + strings.Join(wrapped, "\n") + "\n"
}

// Help screen of the entry in the argparse theme, laid out the way the argparse 3.14 HelpFormatter lays it out
func storeUpdateHelp(prog string, width int, t pm.ArgparseColors) string {
	var b strings.Builder
	b.WriteString(storeUpdateUsage(prog, width, t))
	b.WriteString("\n")
	desc := strings.TrimSpace(storeUpdateWhitespace.ReplaceAllString(windowsStoreUpdateDoc, " "))
	b.WriteString(strings.Join(pystr.TextwrapWrap(desc, pystr.DefaultTextwrap(max(width, 11))), "\n"))
	b.WriteString("\n\n" + t.Heading + "options:" + t.Reset + "\n")
	helpPosition := min(len("--mode {check,download,install}")+2+2, min(24, max(width-20, 4)))
	helpWidth := max(width-helpPosition, 11)
	actionWidth := helpPosition - 2 - 2
	const plainHeader = "-h, --help"
	header := t.ShortOption + "-h" + t.Reset + ", " + t.LongOption + "--help" + t.Reset
	helpLines := pystr.TextwrapWrap("show this help message and exit", pystr.DefaultTextwrap(helpWidth))
	if len(plainHeader) <= actionWidth {
		fmt.Fprintf(&b, "  %s%s  %s\n", header, strings.Repeat(" ", actionWidth-len(plainHeader)), helpLines[0])
	} else {
		fmt.Fprintf(&b, "  %s\n%*s%s\n", header, helpPosition, "", helpLines[0])
	}
	for _, line := range helpLines[1:] {
		fmt.Fprintf(&b, "%*s%s\n", helpPosition, "", line)
	}
	fmt.Fprintf(&b, "  %s--mode%s %s{check,download,install}%s\n", t.LongOption, t.Reset, t.Label, t.Reset)
	fmt.Fprintf(&b, "  %s--hwnd%s %sHWND%s\n", t.LongOption, t.Reset, t.Label, t.Reset)
	return b.String()
}

var storeUpdateWhitespace = regexp.MustCompile(`[ \t\n\r\f\v]+`)
