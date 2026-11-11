package app

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
)

var (
	hfHubLog            = logging.GetLogger("huggingface_hub.utils._http")
	hfRepoAPIRE         = regexp.MustCompile(`^https://[^/]+(/api/(models|datasets|spaces)/(.+)|/(.+)/resolve/(.+))`)
	hfBucketAPIRE       = regexp.MustCompile(`^https?://[^/]+/api/buckets/`)
	hfRatelimitRE       = regexp.MustCompile(`"(\w+)"\s*;\s*r\s*=\s*(\d+)\s*;\s*t\s*=\s*(\d+)`)
	hfRatelimitPolicyRE = regexp.MustCompile(`q\s*=\s*(\d+).*?w\s*=\s*(\d+)`)
	hfRetryStatus       = map[int]bool{408: true, 429: true, 500: true, 502: true, 503: true, 504: true}
	hfLFSHeaders        = map[string]string{"Accept": "application/vnd.git-lfs+json", "Content-Type": "application/vnd.git-lfs+json"}
	hfWarnedTopics      = map[string]bool{}
	hfClient            = pyhttpx.PlainClient(true)
)

// HfHubHTTPError or the subclass a Hub response maps to, carrying the status code
type hfHubHTTPError struct {
	class  string
	msg    string
	status int
}

func (e *hfHubHTTPError) Error() string      { return e.msg }
func (e *hfHubHTTPError) PyTypeName() string { return e.class }

// Hub endpoint from HF_ENDPOINT, the public Hub by default
func hfEndpoint() string {
	endpoint := os.Getenv("HF_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://huggingface.co"
	}
	return strings.TrimRight(endpoint, "/")
}

// Authorization and user agent headers of every Hub call
func hfHeaders(token string) map[string]string {
	return map[string]string{"authorization": "Bearer " + token, "user-agent": "unknown/None; hf_hub/2.4.0; python/3.14"}
}

// One Hub request body and headers
type hfRequest struct {
	method, url string
	headers     map[string]string
	body        []byte
	json        any
}

// Sends a request once, the trace id header added the way the session event hook adds it
func hfSend(ctx context.Context, r hfRequest) (*pyhttpx.Response, error) {
	body := r.body
	headers := map[string]string{}
	for k, v := range r.headers {
		headers[k] = v
	}
	if r.json != nil {
		text, err := pyjson.Dumps(r.json)
		if err != nil {
			return nil, err
		}
		body = []byte(text)
		if _, ok := headerLookup(headers, "Content-Type"); !ok {
			headers["Content-Type"] = "application/json"
		}
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("X-Amzn-Trace-Id") == "" {
		req.Header.Set("X-Amzn-Trace-Id", uuid.NewString())
	}
	return pyhttpx.Do(hfClient, req, 0)
}

// Case insensitive header lookup in a plain map
func headerLookup(h map[string]string, name string) (string, bool) {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// Whether a transport error is one http_backoff retries
func hfRetryableError(err error) bool {
	var te *pyhttpx.TimeoutError
	var ne net.Error
	var ue *url.Error
	return errors.As(err, &te) || errors.As(err, &ne) || errors.As(err, &ue)
}

// Request retried on transient statuses and network errors with exponential backoff, rate limit waits honored
func hfBackoff(ctx context.Context, r hfRequest) (*pyhttpx.Response, error) {
	const maxRetries = 5
	sleep := 1.0
	for tries := 1; ; tries++ {
		var reset *int
		resp, err := hfSend(ctx, r)
		if err != nil {
			if !hfRetryableError(err) {
				return nil, err
			}
			hfHubLog.Warning(fmt.Sprintf("'%s' thrown while requesting %s %s", pyhttpx.ErrorText(err), r.method, r.url))
			if tries > maxRetries {
				return nil, err
			}
		} else {
			if !hfRetryStatus[resp.StatusCode] {
				return resp, nil
			}
			hfHubLog.Warning(fmt.Sprintf("HTTP Error %d thrown while requesting %s %s", resp.StatusCode, r.method, r.url))
			if tries > maxRetries {
				if err := hfRaiseForStatus(resp, r, ""); err != nil {
					return nil, err
				}
				return resp, nil
			}
			if info := hfParseRatelimit(resp.Header); resp.StatusCode == 429 && info != nil && info.remaining == 0 {
				reset = &info.reset
			} else if after, ok := hfParseRetryAfter(resp.Header); ok {
				reset = &after
			}
		}
		var actual float64
		if reset != nil {
			actual = float64(*reset) + 1
			hfHubLog.Warning(fmt.Sprintf("Rate limited. Waiting %ss before retry [Retry %d/%d].", pyrepr.Repr(actual), tries, maxRetries))
		} else {
			actual = sleep
			hfHubLog.Warning(fmt.Sprintf("Retrying in %ss [Retry %d/%d].", pyrepr.Repr(actual), tries, maxRetries))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(actual * float64(time.Second))):
		}
		sleep = math.Min(8, sleep*2)
	}
}

// Parsed RateLimit headers of a Hub response
type hfRatelimitInfo struct {
	resourceType       string
	remaining, reset   int
	limit, windowSecs  int
	hasLimit, hasWinds bool
}

// RateLimit and RateLimit-Policy headers, nil when absent or malformed
func hfParseRatelimit(h http.Header) *hfRatelimitInfo {
	ratelimit, policy := h.Get("RateLimit"), h.Get("RateLimit-Policy")
	if ratelimit == "" {
		return nil
	}
	m := hfRatelimitRE.FindStringSubmatch(ratelimit)
	if m == nil {
		return nil
	}
	info := &hfRatelimitInfo{resourceType: m[1]}
	info.remaining, _ = strconv.Atoi(m[2])
	info.reset, _ = strconv.Atoi(m[3])
	if policy != "" {
		if pm := hfRatelimitPolicyRE.FindStringSubmatch(policy); pm != nil {
			info.limit, _ = strconv.Atoi(pm[1])
			info.windowSecs, _ = strconv.Atoi(pm[2])
			info.hasLimit, info.hasWinds = true, true
		}
	}
	return info
}

// Retry-After seconds when given as digits
func hfParseRetryAfter(h http.Header) (int, bool) {
	value := pystr.Strip(h.Get("Retry-After"))
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	return n, err == nil
}

// Logs each server warning topic once
func hfWarnOnWarningHeaders(h http.Header) {
	for _, w := range h.Values("X-HF-Warning") {
		topic, message := "", w
		if strings.Contains(w, ";") {
			topic, message, _ = strings.Cut(w, ";")
		}
		topic = pystr.Strip(topic)
		if !hfWarnedTopics[topic] {
			if message = pystr.Strip(message); message != "" {
				hfWarnedTopics[topic] = true
				hfHubLog.Warning(message)
			}
		}
	}
}

// Error of a non 2xx Hub response with the server message and request id folded in, nil otherwise
func hfRaiseForStatus(resp *pyhttpx.Response, r hfRequest, endpointName string) error {
	hfWarnOnWarningHeaders(resp.Header)
	if resp.StatusCode < 400 {
		return nil
	}
	errorCode, errorMessage := resp.Header.Get("X-Error-Code"), resp.Header.Get("X-Error-Message")
	_, hasErrorMessage := resp.Header["X-Error-Message"]
	u := resp.URL
	if u == "" {
		u = r.url
	}
	switch {
	case errorCode == "RevisionNotFound":
		return hfFormat("RevisionNotFoundError", fmt.Sprintf("%d Client Error.\n\nRevision Not Found for url: %s.", resp.StatusCode, u), resp)
	case errorCode == "EntryNotFound":
		return hfFormat("RemoteEntryNotFoundError", fmt.Sprintf("%d Client Error.\n\nEntry Not Found for url: %s.", resp.StatusCode, u), resp)
	case errorCode == "GatedRepo":
		return hfFormat("GatedRepoError", fmt.Sprintf("%d Client Error.\n\nCannot access gated repo for url %s.", resp.StatusCode, u), resp)
	case hasErrorMessage && errorMessage == "Access to this resource is disabled.":
		return hfFormat("DisabledRepoError", fmt.Sprintf("%d Client Error.\n\nCannot access repository for url %s.\nAccess to this resource is disabled.", resp.StatusCode, u), resp)
	case errorCode == "RepoNotFound" && hfBucketAPIRE.MatchString(r.url):
		return hfFormat("BucketNotFoundError", fmt.Sprintf("%d Client Error.\n\nBucket Not Found for url: %s.\nPlease make sure you specified the correct bucket id (namespace/name).\nIf the bucket is private, make sure you are authenticated and your token has the required permissions.", resp.StatusCode, u), resp)
	case errorCode == "RepoNotFound" || (resp.StatusCode == 401 && !(hasErrorMessage && errorMessage == "Invalid credentials in Authorization header") && hfRepoAPIRE.MatchString(r.url)):
		return hfFormat("RepositoryNotFoundError", fmt.Sprintf("%d Client Error.\n\nRepository Not Found for url: %s.\nPlease make sure you specified the correct `repo_id` and `repo_type`.\nIf you are trying to access a private or gated repo, make sure you are authenticated and your token has the required permissions.\nFor more details, see https://huggingface.co/docs/huggingface_hub/authentication", resp.StatusCode, u), resp)
	case resp.StatusCode == 400:
		msg := "\n\nBad request:"
		if endpointName != "" {
			msg = "\n\nBad request for " + endpointName + " endpoint:"
		}
		return hfFormat("BadRequestError", msg, resp)
	case resp.StatusCode == 403:
		em := "None"
		if hasErrorMessage {
			em = errorMessage
		}
		return hfFormat("HfHubHTTPError", fmt.Sprintf("\n\n%d Forbidden: %s.\nCannot access content at: %s.\nMake sure your token has the correct permissions.", resp.StatusCode, em, u), resp)
	case resp.StatusCode == 429:
		var msg string
		if info := hfParseRatelimit(resp.Header); info != nil && info.remaining == 0 {
			msg = fmt.Sprintf("\n\n429 Too Many Requests: you have reached your '%s' rate limit.\nRetry after %d seconds", info.resourceType, info.reset)
			if info.hasLimit && info.hasWinds {
				msg += fmt.Sprintf(" (%d/%d requests remaining in current %ds window).", info.remaining, info.limit, info.windowSecs)
			} else {
				msg += "."
			}
			msg += "\nUrl: " + u + "."
		} else {
			msg = "\n\n429 Too Many Requests for url: " + u + "."
		}
		return hfFormat("HfHubHTTPError", msg, resp)
	}
	return hfFormat("HfHubHTTPError", resp.StatusErrorText(u), resp)
}

// Error of the given class with the server errors and the first request id header folded into the message
func hfFormat(class, custom string, resp *pyhttpx.Response) error {
	var serverErrors []string
	if v, ok := resp.Header["X-Error-Message"]; ok && len(v) > 0 {
		serverErrors = append(serverErrors, v[0])
	}
	data, err := pyjson.LoadsBytes(resp.Content)
	if err == nil {
		if obj, ok := data.(*pyjson.Object); ok {
			errVal, hasErr := obj.Get("error")
			desc, hasDesc := obj.Get("error_description")
			hasErr, hasDesc = hasErr && errVal != nil, hasDesc && desc != nil
			if hasErr {
				if list, ok := errVal.([]any); ok {
					for _, item := range list {
						serverErrors = append(serverErrors, pyrepr.Str(item))
					}
				} else if hasDesc {
					serverErrors = append(serverErrors, pyrepr.Str(errVal)+": "+pyrepr.Str(desc))
				} else {
					serverErrors = append(serverErrors, pyrepr.Str(errVal))
				}
			} else if hasDesc {
				serverErrors = append(serverErrors, pyrepr.Str(desc))
			}
			if list, ok := obj.GetOr("errors", nil).([]any); ok {
				for _, item := range list {
					if e, ok := item.(*pyjson.Object); ok {
						if m, ok := e.Get("message"); ok {
							serverErrors = append(serverErrors, pyrepr.Str(m))
						}
					}
				}
			}
		}
	} else {
		var decodeErr *pyjson.DecodeError
		if errors.As(err, &decodeErr) {
			if text := resp.Text(); text != "" && !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") {
				serverErrors = append(serverErrors, text)
			}
		}
	}
	seen := map[string]bool{}
	var unique []string
	for _, line := range serverErrors {
		line = pystr.Strip(line)
		if line != "" && !seen[line] {
			seen[line] = true
			unique = append(unique, line)
		}
	}
	serverMessage := strings.Join(unique, "\n")
	final := custom
	if serverMessage != "" && !strings.Contains(pystr.Lower(custom), pystr.Lower(serverMessage)) {
		if strings.Contains(custom, "\n\n") {
			final += "\n" + serverMessage
		} else {
			final += "\n\n" + serverMessage
		}
	}
	for _, pair := range [][2]string{{"x-request-id", "Request ID"}, {"X-Amzn-Trace-Id", "Amzn Trace ID"}, {"X-Amz-Cf-Id", "Amz CF ID"}} {
		value := resp.Header.Get(pair[0])
		if value == "" {
			continue
		}
		if !strings.Contains(pystr.Lower(final), pystr.Lower(value)) {
			idMsg := " (" + pair[1] + ": " + value + ")"
			if i := strings.Index(final, "\n"); i >= 0 {
				final = final[:i] + idMsg + final[i:]
			} else {
				final += idMsg
			}
		}
		break
	}
	return &hfHubHTTPError{class: class, msg: pystr.Strip(final), status: resp.StatusCode}
}

// Account of the token from the whoami endpoint
func hfWhoami(ctx context.Context, token string) (any, error) {
	r := hfRequest{method: "GET", url: hfEndpoint() + "/api/whoami-v2", headers: hfHeaders(token)}
	resp, err := hfSend(ctx, r)
	if err != nil {
		return nil, err
	}
	if err := hfRaiseForStatus(resp, r, ""); err != nil {
		var hubErr *hfHubHTTPError
		if errors.As(err, &hubErr) && hubErr.status == 401 {
			msg := "Invalid user token."
			if token == hfTokenFromEnvironment() {
				msg += " The token from HF_TOKEN environment variable is invalid. Note that HF_TOKEN takes precedence over `hf auth login`."
			}
			return nil, &hfHubHTTPError{class: hubErr.class, msg: msg, status: 401}
		}
		return nil, err
	}
	return resp.JSON()
}

// Token huggingface_hub reads from the environment
func hfTokenFromEnvironment() string {
	for _, name := range []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN"} {
		if v := pystr.Strip(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// Metadata of a dataset repo, the repo_info call
func hfRepoInfo(ctx context.Context, token, repoID string) error {
	r := hfRequest{method: "GET", url: hfEndpoint() + "/api/datasets/" + repoID, headers: hfHeaders(token)}
	resp, err := hfSend(ctx, r)
	if err != nil {
		return err
	}
	return hfRaiseForStatus(resp, r, "")
}

// Creates a dataset repo, an existing one accepted and a forbidden namespace accepted when the repo is readable
func hfCreateDatasetRepo(ctx context.Context, token, repoID string, private bool) error {
	var organization any
	name := repoID
	if strings.Contains(repoID, "/") {
		parts := strings.Split(repoID, "/")
		if len(parts) != 2 {
			return &appValueError{fmt.Sprintf("too many values to unpack (expected 2, got %d)", len(parts))}
		}
		organization, name = parts[0], parts[1]
	}
	visibility := "public"
	if private {
		visibility = "private"
	}
	r := hfRequest{method: "POST", url: hfEndpoint() + "/api/repos/create", headers: hfHeaders(token),
		json: pyjson.ObjectOf("name", name, "organization", organization, "visibility", visibility, "type", "dataset")}
	var resp *pyhttpx.Response
	for {
		var err error
		if resp, err = hfSend(ctx, r); err != nil {
			return err
		}
		if resp.StatusCode == 409 && strings.Contains(resp.Text(), "Cannot create repo: another conflicting operation is in progress") {
			continue
		}
		break
	}
	if err := hfRaiseForStatus(resp, r, ""); err != nil {
		switch resp.StatusCode {
		case 409:
		case 401, 402, 403:
			if infoErr := hfRepoInfo(ctx, token, repoID); infoErr != nil {
				var hubErr *hfHubHTTPError
				if errors.As(infoErr, &hubErr) {
					return err
				}
				return infoErr
			}
			return nil
		default:
			return err
		}
	}
	data, err := resp.JSON()
	if err != nil {
		return err
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return &pyrepr.FormatError{Class: "TypeError", Msg: "'" + pyrepr.TypeName(data) + "' object is not subscriptable"}
	}
	if _, ok := obj.Get("url"); !ok {
		return &pyrepr.FormatError{Class: "KeyError", Msg: "'url'"}
	}
	return nil
}

// Upload mode, ignore flag and remote oid the preupload endpoint answers for one file
type hfUploadPlan struct {
	mode         string
	shouldIgnore bool
	remoteOID    *string
}

// Asks the Hub how the file uploads, regular or lfs, a zero size file always regular
func hfFetchUploadMode(ctx context.Context, token, repoID, path string, content []byte) (*hfUploadPlan, error) {
	sample := content
	if len(sample) > 512 {
		sample = sample[:512]
	}
	file := pyjson.ObjectOf("path", path, "sample", base64.StdEncoding.EncodeToString(sample), "size", len(content))
	r := hfRequest{method: "POST", url: hfEndpoint() + "/api/datasets/" + repoID + "/preupload/main",
		headers: hfHeaders(token), json: pyjson.ObjectOf("files", []any{file})}
	resp, err := hfBackoff(ctx, r)
	if err != nil {
		return nil, err
	}
	if err := hfRaiseForStatus(resp, r, ""); err != nil {
		return nil, err
	}
	data, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	malformed := &appValueError{"preupload_info is improperly formatted: " + pyrepr.Repr(data)}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil, malformed
	}
	files, ok := obj.GetOr("files", nil).([]any)
	if !ok {
		return nil, malformed
	}
	var plan *hfUploadPlan
	for _, item := range files {
		f, ok := item.(*pyjson.Object)
		if !ok {
			return nil, malformed
		}
		p, pok := f.GetOr("path", nil).(string)
		mode, mok := f.GetOr("uploadMode", nil).(string)
		ignore, iok := f.GetOr("shouldIgnore", nil).(bool)
		if !pok || !mok || !iok || (mode != "lfs" && mode != "regular") {
			return nil, malformed
		}
		if p != path {
			continue
		}
		plan = &hfUploadPlan{mode: mode, shouldIgnore: ignore}
		if oid, ok := f.GetOr("oid", nil).(string); ok {
			plan.remoteOID = &oid
		}
	}
	if plan == nil {
		return nil, &pyrepr.FormatError{Class: "KeyError", Msg: pyrepr.Repr(path)}
	}
	if len(content) == 0 {
		plan.mode = "regular"
	}
	return plan, nil
}

// Uploads one LFS object through the batch API, basic or multipart, then verifies it
func hfUploadLFS(ctx context.Context, token, repoID string, content []byte, oid string) error {
	headers := map[string]string{}
	for k, v := range hfLFSHeaders {
		headers[k] = v
	}
	for k, v := range hfHeaders(token) {
		headers[k] = v
	}
	r := hfRequest{method: "POST", url: hfEndpoint() + "/datasets/" + repoID + ".git/info/lfs/objects/batch", headers: headers,
		json: pyjson.ObjectOf("operation", "upload", "transfers", []any{"basic", "multipart"},
			"objects", []any{pyjson.ObjectOf("oid", oid, "size", len(content))}, "hash_algo", "sha256",
			"ref", pyjson.ObjectOf("name", "main"))}
	resp, err := hfBackoff(ctx, r)
	if err != nil {
		return err
	}
	if err := hfRaiseForStatus(resp, r, ""); err != nil {
		return err
	}
	data, err := resp.JSON()
	if err != nil {
		return err
	}
	info, _ := data.(*pyjson.Object)
	objects, ok := info.GetOr("objects", nil).([]any)
	if info == nil || !ok {
		return &appValueError{"Malformed response from server"}
	}
	var actions []*pyjson.Object
	var batchErrors []string
	for _, item := range objects {
		obj, _ := item.(*pyjson.Object)
		if obj == nil {
			return &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + pyrepr.TypeName(item) + "' object has no attribute 'get'"}
		}
		if _, isErr := obj.Get("error"); isErr {
			if err := hfValidateBatchError(obj); err != nil {
				return err
			}
			errInfo := obj.Vals["error"].(*pyjson.Object)
			batchErrors = append(batchErrors, fmt.Sprintf("Encountered error for file with OID %s: `%s", pyrepr.Str(obj.Vals["oid"]), pyrepr.Str(errInfo.Vals["message"])))
			continue
		}
		if err := hfValidateBatchActions(obj); err != nil {
			return err
		}
		actions = append(actions, obj)
	}
	if len(batchErrors) > 0 {
		return &appValueError{"LFS batch API returned errors:\n" + strings.Join(batchErrors, "\n")}
	}
	for _, action := range actions {
		if err := hfLFSUpload(ctx, token, content, oid, action); err != nil {
			return err
		}
	}
	return nil
}

// Whether a JSON value passes isinstance int, bools included
func hfIsInt(v any) bool {
	switch v.(type) {
	case int, int64, *big.Int, bool:
		return true
	}
	return false
}

// Refuses an action without a string href or with a non dict header
func hfValidateLFSAction(action any) error {
	obj, ok := action.(*pyjson.Object)
	if ok {
		_, hrefOK := obj.GetOr("href", nil).(string)
		header := obj.GetOr("header", nil)
		_, headerOK := header.(*pyjson.Object)
		if hrefOK && (header == nil || headerOK) {
			return nil
		}
	}
	return &appValueError{"lfs_action is improperly formatted"}
}

// Refuses a batch object without oid and size or with malformed actions
func hfValidateBatchActions(obj *pyjson.Object) error {
	if _, ok := obj.GetOr("oid", nil).(string); !ok || !hfIsInt(obj.GetOr("size", nil)) {
		return &appValueError{"lfs_batch_actions is improperly formatted"}
	}
	actions, _ := obj.GetOr("actions", pyjson.NewObject()).(*pyjson.Object)
	if actions == nil {
		return &pyrepr.FormatError{Class: "AttributeError", Msg: "object has no attribute 'get'"}
	}
	for _, name := range []string{"upload", "verify"} {
		if a := actions.GetOr(name, nil); a != nil {
			if err := hfValidateLFSAction(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// Refuses a batch error object without oid, size, message and code
func hfValidateBatchError(obj *pyjson.Object) error {
	if _, ok := obj.GetOr("oid", nil).(string); !ok || !hfIsInt(obj.GetOr("size", nil)) {
		return &appValueError{"lfs_batch_error is improperly formatted"}
	}
	info, ok := obj.GetOr("error", nil).(*pyjson.Object)
	if !ok {
		return &appValueError{"lfs_batch_error is improperly formatted"}
	}
	if _, ok := info.GetOr("message", nil).(string); !ok || !hfIsInt(info.GetOr("code", nil)) {
		return &appValueError{"lfs_batch_error is improperly formatted"}
	}
	return nil
}

// Endpoint of a Hub URL rewritten onto the configured endpoint
func hfFixEndpointInURL(u string) string {
	endpoint := hfEndpoint()
	if endpoint != "https://huggingface.co" && strings.HasPrefix(u, "https://huggingface.co") {
		return endpoint + strings.TrimPrefix(u, "https://huggingface.co")
	}
	return u
}

// Performs one batch action, single PUT or the multipart protocol, then the verify call
func hfLFSUpload(ctx context.Context, token string, content []byte, oid string, action *pyjson.Object) error {
	actions, _ := action.GetOr("actions", nil).(*pyjson.Object)
	if actions == nil {
		return nil
	}
	upload, ok := actions.Get("upload")
	if !ok {
		return &pyrepr.FormatError{Class: "KeyError", Msg: "'upload'"}
	}
	if err := hfValidateLFSAction(upload); err != nil {
		return err
	}
	uploadObj := upload.(*pyjson.Object)
	verify := actions.GetOr("verify", nil)
	header, _ := uploadObj.GetOr("header", pyjson.NewObject()).(*pyjson.Object)
	if header == nil {
		header = pyjson.NewObject()
	}
	uploadURL := hfFixEndpointInURL(uploadObj.Vals["href"].(string))
	if chunk, ok := header.Get("chunk_size"); ok && chunk != nil {
		size, err := pyintOfChunk(chunk)
		if err != nil {
			return err
		}
		if err := hfUploadMultiPart(ctx, content, oid, header, size, uploadURL); err != nil {
			return err
		}
	} else {
		r := hfRequest{method: "PUT", url: uploadURL, body: content}
		resp, err := hfBackoff(ctx, r)
		if err != nil {
			return err
		}
		if err := hfRaiseForStatus(resp, r, ""); err != nil {
			return err
		}
	}
	if verify != nil {
		if err := hfValidateLFSAction(verify); err != nil {
			return err
		}
		r := hfRequest{method: "POST", url: hfFixEndpointInURL(verify.(*pyjson.Object).Vals["href"].(string)),
			headers: hfHeaders(token), json: pyjson.ObjectOf("oid", oid, "size", len(content))}
		resp, err := hfBackoff(ctx, r)
		if err != nil {
			return err
		}
		return hfRaiseForStatus(resp, r, "")
	}
	return nil
}

// int of a chunk_size header value, the batch endpoint error for anything else
func pyintOfChunk(v any) (int, error) {
	bad := &appValueError{fmt.Sprintf("Malformed response from LFS batch endpoint: `chunk_size` should be an integer. Got '%s'.", pyrepr.Str(v))}
	switch t := v.(type) {
	case string:
		n, ok := pystr.IntText(t)
		if !ok || !n.IsInt64() {
			return 0, bad
		}
		return int(n.Int64()), nil
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		return int(t), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	}
	return 0, bad
}

// Uploads the parts to their presigned URLs in part order then posts the completion payload
func hfUploadMultiPart(ctx context.Context, content []byte, oid string, header *pyjson.Object, chunkSize int, uploadURL string) error {
	type part struct {
		n   int
		url string
	}
	var parts []part
	for _, k := range header.Keys {
		if k == "" || strings.Trim(k, "0123456789") != "" {
			continue
		}
		n, _ := strconv.Atoi(k)
		parts = append(parts, part{n, pyrepr.Str(header.Vals[k])})
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].n < parts[j].n })
	if chunkSize == 0 {
		return &pyrepr.FormatError{Class: "ZeroDivisionError", Msg: "division by zero"}
	}
	if len(parts) != int(math.Ceil(float64(len(content))/float64(chunkSize))) {
		return &appValueError{"Invalid server response to upload large LFS file"}
	}
	var completed []any
	for i, p := range parts {
		start := chunkSize * i
		end := start + chunkSize
		if start > len(content) {
			start = len(content)
		}
		if end > len(content) {
			end = len(content)
		}
		r := hfRequest{method: "PUT", url: p.url, body: content[start:end]}
		resp, err := hfBackoff(ctx, r)
		if err != nil {
			return err
		}
		if err := hfRaiseForStatus(resp, r, ""); err != nil {
			return err
		}
		etag := resp.Header.Get("etag")
		if etag == "" {
			shown := "None"
			if _, ok := resp.Header["Etag"]; ok {
				shown = ""
			}
			return &appValueError{fmt.Sprintf("Invalid etag (`%s`) returned for part %d", shown, i+1)}
		}
		completed = append(completed, pyjson.ObjectOf("partNumber", i+1, "etag", etag))
	}
	r := hfRequest{method: "POST", url: uploadURL, headers: hfLFSHeaders, json: pyjson.ObjectOf("oid", oid, "parts", completed)}
	resp, err := hfBackoff(ctx, r)
	if err != nil {
		return err
	}
	return hfRaiseForStatus(resp, r, "")
}

// Git blob sha1 of content, the oid of a regular file
func hfGitHash(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// Commits one file to a dataset repo on main through preupload, LFS when asked, and the commit endpoint
func hfUploadFile(ctx context.Context, token, repoID, pathInRepo string, content []byte, commitMessage string) error {
	if commitMessage == "" {
		return &appValueError{"`commit_message` can't be empty, please pass a value."}
	}
	plan, err := hfFetchUploadMode(ctx, token, repoID, pathInRepo, content)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	if plan.mode == "lfs" && !plan.shouldIgnore {
		if err := hfUploadLFS(ctx, token, repoID, content, oid); err != nil {
			return err
		}
	}
	localOID := oid
	if plan.mode == "regular" {
		localOID = hfGitHash(content)
	}
	if plan.remoteOID != nil && *plan.remoteOID == localOID {
		hfHubLog.Warning("No files have been modified since last commit. Skipping to prevent empty commit.")
		return hfRepoInfo(ctx, token, repoID)
	}
	lines := []any{pyjson.ObjectOf("key", "header", "value", pyjson.ObjectOf("summary", commitMessage, "description", ""))}
	if !plan.shouldIgnore {
		if plan.mode == "regular" {
			lines = append(lines, pyjson.ObjectOf("key", "file", "value", pyjson.ObjectOf(
				"content", base64.StdEncoding.EncodeToString(content), "path", pathInRepo, "encoding", "base64")))
		} else {
			lines = append(lines, pyjson.ObjectOf("key", "lfsFile", "value", pyjson.ObjectOf(
				"path", pathInRepo, "algo", "sha256", "oid", oid, "size", len(content))))
		}
	}
	var body bytes.Buffer
	for _, line := range lines {
		text, err := pyjson.Dumps(line)
		if err != nil {
			return err
		}
		body.WriteString(text + "\n")
	}
	headers := map[string]string{"Content-Type": "application/x-ndjson"}
	for k, v := range hfHeaders(token) {
		headers[k] = v
	}
	r := hfRequest{method: "POST", url: hfEndpoint() + "/api/datasets/" + repoID + "/commit/main", headers: headers, body: body.Bytes()}
	resp, err := hfSend(ctx, r)
	if err != nil {
		return err
	}
	if err := hfRaiseForStatus(resp, r, "commit"); err != nil {
		return err
	}
	data, err := resp.JSON()
	if err != nil {
		return err
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return &pyrepr.FormatError{Class: "TypeError", Msg: "'" + pyrepr.TypeName(data) + "' object is not subscriptable"}
	}
	for _, key := range []string{"commitUrl", "commitOid"} {
		if _, ok := obj.Get(key); !ok {
			return &pyrepr.FormatError{Class: "KeyError", Msg: "'" + key + "'"}
		}
	}
	return nil
}
