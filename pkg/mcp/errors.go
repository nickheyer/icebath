package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"syscall"
)

// InvalidMCPURLError is a remote server url that is not a parseable http or https URL
type InvalidMCPURLError struct{ Msg string }

func (e *InvalidMCPURLError) Error() string { return e.Msg }

// NonMCPEndpointError is a 2xx answer with a non MCP content type, retrying always gets the same page
type NonMCPEndpointError struct{ Msg string }

func (e *NonMCPEndpointError) Error() string { return e.Msg }

// Non retryable error for an HTTP URL that served a non MCP page
func newNonMCPEndpointError(msg string) *NonMCPEndpointError { return &NonMCPEndpointError{Msg: msg} }

// fileNotFoundError is a missing file whose message names the server, matching fs.ErrNotExist
type fileNotFoundError struct{ msg string }

func (e *fileNotFoundError) Error() string { return e.msg }

func (e *fileNotFoundError) Unwrap() error { return fs.ErrNotExist }

// Statuses an SSE only server or its balancer answers the chunked initialize POST with
var streamableRejectStatuses = map[int]bool{400: true, 405: true, 406: true, 411: true}

// Marker of the opaque internal error a non 2xx without a JSON-RPC body turns into
const opaqueHTTPErrorMarker = "server returned an error response"

// Children of a group error, nil for anything else
func groupMembers(err error) []error {
	g, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return nil
	}
	var out []error
	for _, sub := range g.Unwrap() {
		if sub != nil {
			out = append(out, sub)
		}
	}
	return out
}

// Reports a KeyboardInterrupt or SystemExit style error that ends the process
func isProcessExit(err error) bool {
	name := errTypeName(err)
	return name == "SystemExit" || name == "KeyboardInterrupt"
}

// First process exit leaf of a group tree in depth first order, nil when none
func firstProcessExitLeaf(err error) error {
	for _, sub := range groupMembers(err) {
		if groupMembers(sub) != nil {
			if leaf := firstProcessExitLeaf(sub); leaf != nil {
				return leaf
			}
		} else if isProcessExit(sub) {
			return sub
		}
	}
	return nil
}

// Root cause leaf of group wrappers preferring non cancellation members, a process exit leaf panics instead
func unwrapExceptionGroup(err error) error {
	for {
		subs := groupMembers(err)
		if len(subs) == 0 {
			return err
		}
		if leaf := firstProcessExitLeaf(err); leaf != nil {
			panic(leaf)
		}
		next := subs[0]
		for _, sub := range subs {
			if !containsOnlyCancellation(sub) {
				next = sub
				break
			}
		}
		err = next
	}
}

// True for a cancellation or a group holding nothing but cancellations
func containsOnlyCancellation(err error) bool {
	if subs := groupMembers(err); subs != nil {
		for _, sub := range subs {
			if !containsOnlyCancellation(sub) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, context.Canceled)
}

// True when a Streamable HTTP connect failure is a 400 family rejection or the opaque internal error it maps to
func isStreamableHTTPRejection(err error) bool {
	root := unwrapExceptionGroup(err)
	var hse *HTTPStatusError
	if errors.As(root, &hse) && streamableRejectStatuses[hse.Status] {
		return true
	}
	var rpc *RPCError
	return errors.As(root, &rpc) && rpc.Code == -32603 && strings.Contains(strings.ToLower(root.Error()), opaqueHTTPErrorMarker)
}

// Root cause text of an HTTP connect failure with the recorded rejection appended to the opaque internal error
func describeHTTPFailure(err error, rejection HTTPRejection) string {
	root := unwrapExceptionGroup(err)
	text := root.Error()
	var rpc *RPCError
	opaque := errors.As(root, &rpc) && rpc.Code == -32603 && strings.Contains(strings.ToLower(text), opaqueHTTPErrorMarker)
	detail := rejection.Describe()
	if !opaque || detail == "" {
		return text
	}
	return fmt.Sprintf("%s (%s)", text, detail)
}

// Leading space plus the sanitized guidance of an OAuth registration root cause, empty for anything else
func authErrorDetail(err error) string {
	root := unwrapExceptionGroup(err)
	var reg *OAuthRegistrationError
	if !errors.As(root, &reg) {
		return ""
	}
	if text := strings.TrimSpace(root.Error()); text != "" {
		return " " + sanitizeError(text)
	}
	return ""
}

// Generic tool error naming the root cause type and text instead of the group wrapper
func mcpCallFailedMessage(err error) string {
	root := unwrapExceptionGroup(err)
	return sanitizeError(fmt.Sprintf("MCP call failed: %s: %s", errTypeName(root), excStr(root)))
}

// Error a connection wrapper stands for, the node ABI error it re-raises or the wrapper itself
func connectionErrorRoot(err error) error {
	ce, ok := err.(*mcpConnectionError)
	if !ok {
		return err
	}
	for _, member := range groupMembers(ce.cause) {
		if abi, ok := member.(*NodeABIMismatchError); ok && abi.Msg == ce.msg {
			return abi
		}
	}
	return ce
}

// Wrapper chain of err in pre order, a connection error ends its branch like a Python ConnectionError
func connectionScopedNodes(err error) []error {
	stack := []error{err}
	seen := map[errKey]bool{}
	var ordered []error
	for len(stack) > 0 && len(ordered) < excTraversalMaxNodes {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if key, ok := errIdentity(current); ok {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		ordered = append(ordered, current)
		if _, ok := current.(*mcpConnectionError); ok {
			continue
		}
		children := excChildren(current)
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, children[i])
		}
	}
	return ordered
}

// Assigns node to target when its type fits or its As method accepts, without unwrapping
func nodeAs(node error, target any) bool {
	dst := reflect.ValueOf(target).Elem()
	if reflect.TypeOf(node).AssignableTo(dst.Type()) {
		dst.Set(reflect.ValueOf(node))
		return true
	}
	x, ok := node.(interface{ As(any) bool })
	return ok && x.As(target)
}

// True when a node of the connection scoped chain matches target
func anyNodeAs(nodes []error, target any) bool {
	for _, node := range nodes {
		if nodeAs(node, target) {
			return true
		}
	}
	return false
}

// Permanent for auth, non MCP endpoints, invalid URLs, missing commands and foreign Node addons, transient otherwise
func classifyMCPFailure(err error) string {
	root := connectionErrorRoot(unwrapExceptionGroup(err))
	if isAuthError(root) {
		return "permanent"
	}
	nodes := connectionScopedNodes(root)
	var nonMCP *NonMCPEndpointError
	var badURL *InvalidMCPURLError
	var abi *NodeABIMismatchError
	if anyNodeAs(nodes, &nonMCP) || anyNodeAs(nodes, &badURL) || anyNodeAs(nodes, &abi) {
		return "permanent"
	}
	for _, node := range nodes {
		if nodeIsFileNotFound(node) || missingFileName(node) != "" {
			return "permanent"
		}
		if hse, ok := node.(*HTTPStatusError); ok && (hse.Status == 401 || hse.Status == 403) {
			return "permanent"
		}
	}
	return "transient"
}

// Stripped http or https URL of a remote server, an InvalidMCPURLError naming the server otherwise
func validatedRemoteMCPURL(serverName string, raw any) (string, error) {
	bad := func(detail string) error {
		return &InvalidMCPURLError{Msg: fmt.Sprintf("Invalid MCP URL for '%s': %s", serverName, detail)}
	}
	s, ok := raw.(string)
	if !ok {
		return "", bad("expected a string, got " + pyTypeName(raw))
	}
	stripped := pyStrip(s)
	if stripped == "" {
		return "", bad("empty url")
	}
	parsed, err := pyURLParse(stripped)
	if err != nil {
		return "", bad(fmt.Sprintf("%s (%s)", pyReprStr(stripped), err.Error()))
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return "", bad(fmt.Sprintf("scheme must be http or https, got %s (%s)", pyReprStr(parsed.Scheme), pyReprStr(stripped)))
	}
	if parsed.Netloc == "" {
		return "", bad(fmt.Sprintf("missing host (%s)", pyReprStr(stripped)))
	}
	if netlocHostname(parsed.Netloc) == "" {
		return "", bad(fmt.Sprintf("missing hostname (%s)", pyReprStr(stripped)))
	}
	return stripped, nil
}

// Fails fast with an InvalidMCPURLError when a remote server url is not http or https with a host
func validateRemoteMCPURL(serverName string, raw any) error {
	_, err := validatedRemoteMCPURL(serverName, raw)
	return err
}

// Nodes visited before an error tree walk stops
const excTraversalMaxNodes = 10_000

// Group members followed by the wrapped cause, so a chain is never skipped
func excChildren(err error) []error {
	out := groupMembers(err)
	if u, ok := err.(interface{ Unwrap() error }); ok {
		if cause := u.Unwrap(); cause != nil {
			out = append(out, cause)
		}
	}
	return out
}

// errKey is the identity of a pointer shaped error
type errKey struct {
	t reflect.Type
	p uintptr
}

// Identity of a pointer shaped error for cycle detection, ok false for value errors
func errIdentity(err error) (errKey, bool) {
	v := reflect.ValueOf(err)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return errKey{v.Type(), v.Pointer()}, true
	}
	return errKey{}, false
}

// Pre order left to right walk of an error tree and chain, each node once, bounded by the traversal budget
func iterExceptionNodes(err error) []error {
	stack := []error{err}
	seen := map[errKey]bool{}
	var ordered []error
	for len(stack) > 0 && len(ordered) < excTraversalMaxNodes {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if key, ok := errIdentity(current); ok {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		ordered = append(ordered, current)
		children := excChildren(current)
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, children[i])
		}
	}
	return ordered
}

// Python style missing file text a path is recovered from
var noSuchFileQuoted = regexp.MustCompile(`No such file or directory: '([^']+)'`)

// Executable names whose absence points at a missing Node.js toolchain
var nodeToolchainBins = map[string]bool{"npx": true, "npm": true, "node": true}

// True when this node itself is a missing file error, its wrapped chain is walked separately
func nodeIsFileNotFound(node error) bool {
	switch e := node.(type) {
	case *fileNotFoundError:
		return true
	case syscall.Errno:
		return e == syscall.ENOENT
	}
	return node == fs.ErrNotExist || node == exec.ErrNotFound
}

// Missing file or executable a node names, empty when the node is not a missing file
func missingFileName(node error) string {
	switch e := node.(type) {
	case *fs.PathError:
		if e.Err == syscall.ENOENT || e.Err == fs.ErrNotExist {
			return e.Path
		}
		return ""
	case *exec.Error:
		if e.Err == exec.ErrNotFound || e.Err == fs.ErrNotExist || e.Err == syscall.ENOENT {
			return e.Name
		}
		return ""
	}
	if !nodeIsFileNotFound(node) {
		return ""
	}
	if m := noSuchFileQuoted.FindStringSubmatch(node.Error()); m != nil {
		return m[1]
	}
	return ""
}

// Short actionable message for nested connection errors, naming a missing executable or a foreign Node addon
func formatConnectError(err error) string {
	nodes := iterExceptionNodes(err)
	for _, node := range nodes {
		if abi, ok := node.(*NodeABIMismatchError); ok {
			return sanitizeError(abi.Error())
		}
	}
	missing := ""
	for _, node := range nodes {
		if missing = missingFileName(node); missing != "" {
			break
		}
	}
	if missing == "" {
		var messages []string
		seen := map[string]bool{}
		add := func(text string) {
			if seen[text] {
				return
			}
			for _, prior := range messages {
				if strings.Contains(prior, text) {
					return
				}
			}
			seen[text] = true
			messages = append(messages, text)
		}
		for _, node := range nodes {
			text := ""
			if groupMembers(node) == nil {
				text = strings.TrimSpace(node.Error())
			}
			if text != "" {
				add(text)
			} else if len(excChildren(node)) == 0 {
				add(errTypeName(node))
			}
		}
		if len(messages) == 0 {
			messages = []string{errTypeName(err)}
		}
		if len(messages) > 3 {
			messages = messages[:3]
		}
		return sanitizeError(strings.Join(messages, "; "))
	}
	message := fmt.Sprintf("missing executable '%s'", missing)
	if nodeToolchainBins[pyBasename(missing)] {
		message += " (ensure Node.js is installed and PATH includes its bin directory, " +
			"or set mcp_servers.<name>.command to an absolute path and include " +
			"that directory in mcp_servers.<name>.env.PATH)"
	}
	return sanitizeError(message)
}

// True for an MCP OAuth failure, an HTTP status error counts only with status 401, a connection error never
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	for _, node := range connectionScopedNodes(err) {
		if hse, ok := node.(*HTTPStatusError); ok {
			return hse.Status == 401
		}
		var flow *OAuthFlowError
		var token *OAuthTokenError
		var nonInteractive *OAuthNonInteractiveError
		if nodeAs(node, &flow) || nodeAs(node, &token) || nodeAs(node, &nonInteractive) {
			return true
		}
	}
	return false
}

// Lower cased substrings meaning the server side transport session expired while the token stays valid
var sessionExpiredMarkers = []string{
	"invalid or expired session", "expired session", "session expired", "session not found",
	"unknown session", "session terminated", "closedresourceerror", "closed resource",
	"transport is closed", "connection closed", "broken pipe", "end of file"}

// Stream errors that end a transport, often message less, matched by identity
func isTransportStreamError(err error) bool {
	switch err {
	case io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, net.ErrClosed, os.ErrClosed, syscall.EPIPE, ErrSessionExpired:
		return true
	}
	return false
}

// True when text holds any of the markers
func containsAnyMarker(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// True when any node looks like a transport session expiry and no node is a user interrupt
func isSessionExpiredError(err error) bool {
	if err == nil {
		return false
	}
	found := false
	for _, current := range iterExceptionNodes(err) {
		if current == ErrInterrupted {
			return false
		}
		found = found || isTransportStreamError(current) || containsAnyMarker(strings.ToLower(current.Error()), sessionExpiredMarkers)
	}
	return found
}
