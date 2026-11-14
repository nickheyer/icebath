package yuanbao

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	UploadInfoPath   = "/api/resource/genUploadInfo"
	DefaultAPIDomain = "yuanbao.tencent.com"
	DefaultMaxSizeMB = 50
)

var mimeToImageFormat = map[string]int64{
	"image/jpeg": 1, "image/jpg": 1, "image/gif": 2, "image/png": 3, "image/bmp": 4,
	"image/webp": 255, "image/heic": 255, "image/tiff": 255,
}

var extToMIME = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
	".bmp": "image/bmp", ".heic": "image/heic", ".tiff": "image/tiff", ".ico": "image/x-icon",
	".pdf": "application/pdf", ".doc": "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".txt":  "text/plain", ".zip": "application/zip", ".tar": "application/x-tar", ".gz": "application/gzip",
	".mp3": "audio/mpeg", ".mp4": "video/mp4", ".wav": "audio/wav", ".ogg": "audio/ogg", ".webm": "video/webm",
}

var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true, ".heic": true, ".tiff": true, ".ico": true}

func ext(filename string) string {
	_, e := pypath.Splitext(filename)
	return pystr.Lower(e)
}

// GuessMIMEType maps a file extension to its MIME type, octet-stream when unknown
func GuessMIMEType(filename string) string {
	if m, ok := extToMIME[ext(filename)]; ok {
		return m
	}
	return "application/octet-stream"
}

// IsImage reports an image MIME prefix or an image extension
func IsImage(filename, mimeType string) bool {
	return strings.HasPrefix(mimeType, "image/") || imageExts[ext(filename)]
}

// GetImageFormat is the TIM image format number, 255 when unknown
func GetImageFormat(mimeType string) int64 {
	if f, ok := mimeToImageFormat[pystr.Lower(mimeType)]; ok {
		return f
	}
	return 255
}

// MD5Hex is the hex md5 digest of the bytes
func MD5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// GenerateFileID returns 32 random hex characters
func GenerateFileID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ImageSize is the width and height parsed from an image header
type ImageSize struct{ Width, Height int64 }

func parsePNGSize(buf []byte) *ImageSize {
	if len(buf) < 24 || !bytes.Equal(buf[:4], []byte("\x89PNG")) {
		return nil
	}
	return &ImageSize{int64(binary.BigEndian.Uint32(buf[16:20])), int64(binary.BigEndian.Uint32(buf[20:24]))}
}

func parseJPEGSize(buf []byte) *ImageSize {
	if len(buf) < 4 || buf[0] != 0xFF || buf[1] != 0xD8 {
		return nil
	}
	i := 2
	for i < len(buf)-9 {
		switch {
		case buf[i] != 0xFF:
			i++
		case buf[i+1] == 0xC0 || buf[i+1] == 0xC2:
			h, w := binary.BigEndian.Uint16(buf[i+5:i+7]), binary.BigEndian.Uint16(buf[i+7:i+9])
			return &ImageSize{int64(w), int64(h)}
		case i+3 < len(buf):
			i += 2 + int(binary.BigEndian.Uint16(buf[i+2:i+4]))
		default:
			return nil
		}
	}
	return nil
}

func parseGIFSize(buf []byte) *ImageSize {
	if len(buf) < 10 || (string(buf[:6]) != "GIF87a" && string(buf[:6]) != "GIF89a") {
		return nil
	}
	return &ImageSize{int64(binary.LittleEndian.Uint16(buf[6:8])), int64(binary.LittleEndian.Uint16(buf[8:10]))}
}

func le24(b []byte) int64 { return int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16 }

func parseWebPSize(buf []byte) *ImageSize {
	if len(buf) < 16 || string(buf[:4]) != "RIFF" || string(buf[8:12]) != "WEBP" {
		return nil
	}
	switch chunk := string(buf[12:16]); {
	case chunk == "VP8 " && len(buf) >= 30 && bytes.Equal(buf[23:26], []byte{0x9d, 0x01, 0x2a}):
		w, h := binary.LittleEndian.Uint16(buf[26:28]), binary.LittleEndian.Uint16(buf[28:30])
		return &ImageSize{int64(w & 0x3FFF), int64(h & 0x3FFF)}
	case chunk == "VP8L" && len(buf) >= 25 && buf[20] == 0x2F:
		bits := binary.LittleEndian.Uint32(buf[21:25])
		return &ImageSize{int64(bits&0x3FFF) + 1, int64((bits>>14)&0x3FFF) + 1}
	case chunk == "VP8X" && len(buf) >= 30:
		return &ImageSize{le24(buf[24:27]) + 1, le24(buf[27:30]) + 1}
	}
	return nil
}

// ParseImageSize reads the width and height of a PNG, JPEG, GIF or WebP header, nil when unrecognized
func ParseImageSize(data []byte) *ImageSize {
	for _, parse := range []func([]byte) *ImageSize{parsePNGSize, parseJPEGSize, parseGIFSize, parseWebPSize} {
		if s := parse(data); s != nil {
			return s
		}
	}
	return nil
}

// Unwraps the url.Error around a redirect guard failure so the ValueError text stands alone
func requestError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) && strings.HasPrefix(uerr.Err.Error(), "Blocked redirect to private/internal address: ") {
		return valueError(uerr.Err.Error())
	}
	return fmt.Errorf("%s", pyhttpx.ErrorText(err))
}

// DownloadURL fetches a public URL behind the SSRF guard within the size cap and returns the bytes and content type
func DownloadURL(ctx context.Context, env *tools.Env, rawURL string, maxSizeMB int64) ([]byte, string, error) {
	if !tools.IsSafeURL(env, rawURL) {
		return nil, "", valueError(fmt.Sprintf("Blocked unsafe URL (SSRF protection): %s", rawURL))
	}
	maxBytes := maxSizeMB * 1024 * 1024
	client := tools.NewSSRFSafeClient(env, 30*time.Second, true)
	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	head, err := pyhttpx.Do(client, headReq, 30*time.Second)
	if err != nil {
		return nil, "", requestError(err)
	}
	lengthValue := any(pyhttpx.HeaderGet(head.Header, "content-length", ""))
	if !pyrepr.Truthy(lengthValue) {
		lengthValue = int64(0)
	}
	contentLength, err := pyint.Of(lengthValue)
	if err != nil {
		return nil, "", err
	}
	if contentLength.Sign() > 0 && contentLength.Cmp(big.NewInt(maxBytes)) > 0 {
		mb, _ := new(big.Float).Quo(new(big.Float).SetInt(contentLength), big.NewFloat(1024*1024)).Float64()
		return nil, "", valueError(fmt.Sprintf("文件过大: %.1f MB > %d MB", mb, maxSizeMB))
	}
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := pyhttpx.Stream(client, getReq, 30*time.Second)
	if err != nil {
		return nil, "", requestError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("%s", pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.URL, resp.Header.Get("Location")))
	}
	contentType := pystr.Strip(strings.SplitN(pyhttpx.HeaderGet(resp.Header, "content-type", ""), ";", 2)[0])
	var out []byte
	buf := make([]byte, 65536)
	for {
		n, rerr := io.ReadFull(resp.Body, buf)
		if n > 0 {
			if int64(len(out)+n) > maxBytes {
				return nil, "", valueError(fmt.Sprintf("文件过大: 已超过 %d MB 限制", maxSizeMB))
			}
			out = append(out, buf[:n]...)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, "", requestError(rerr)
		}
	}
	return out, contentType, nil
}

func hmacSHA1Hex(key, msg string) string {
	m := hmac.New(sha1.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// Lowercased keys with fully percent encoded values in lexical order of the pairs
func sortedKV(d [][2]string) [][2]string {
	out := make([][2]string, len(d))
	for i, kv := range d {
		out[i] = [2]string{pystr.Lower(kv[0]), pyurl.Quote(kv[1], "")}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}

// CosSign builds the q-sign-algorithm=sha1 COS Authorization header from temporary credentials
func CosSign(method, signPath string, params, headers [][2]string, secretID, secretKey string, startTime, expireSeconds *big.Int) string {
	qSignTime := startTime.String() + ";" + new(big.Int).Add(startTime, expireSeconds).String()
	signKey := hmacSHA1Hex(secretKey, qSignTime)
	sortedParams, sortedHeaders := sortedKV(params), sortedKV(headers)
	kv := func(pairs [][2]string) string {
		parts := make([]string, len(pairs))
		for i, p := range pairs {
			parts[i] = p[0] + "=" + p[1]
		}
		return strings.Join(parts, "&")
	}
	keys := func(pairs [][2]string) string {
		parts := make([]string, len(pairs))
		for i, p := range pairs {
			parts[i] = p[0]
		}
		return strings.Join(parts, ";")
	}
	httpString := strings.Join([]string{pystr.Lower(method), signPath, kv(sortedParams), kv(sortedHeaders), ""}, "\n")
	digest := sha1.Sum([]byte(httpString))
	stringToSign := strings.Join([]string{"sha1", qSignTime, hex.EncodeToString(digest[:]), ""}, "\n")
	return fmt.Sprintf("q-sign-algorithm=sha1&q-ak=%s&q-sign-time=%s&q-key-time=%s&q-header-list=%s&q-url-param-list=%s&q-signature=%s",
		secretID, qSignTime, qSignTime, keys(sortedHeaders), keys(sortedParams), hmacSHA1Hex(signKey, stringToSign))
}

// Swappable clock and HTTP client for the COS calls
var (
	unixNow     = func() int64 { return time.Now().Unix() }
	mediaClient = func() *http.Client { return pyhttpx.PlainClient(false) }
)

// GetCOSCredentials calls genUploadInfo for the temporary COS keys and upload target
func GetCOSCredentials(ctx context.Context, appKey, apiDomain, token, filename string, fileID *string, botID, routeEnv string) (*pyjson.Object, error) {
	xid := botID
	if xid == "" {
		xid = appKey
	}
	id := ""
	if fileID != nil {
		id = *fileID
	} else {
		generated, err := GenerateFileID()
		if err != nil {
			return nil, err
		}
		id = generated
	}
	body := pyjson.NewObject()
	body.Set("fileName", filename)
	body.Set("fileId", id)
	body.Set("docFrom", "localDoc")
	body.Set("docOpenId", "")
	payload, err := pyjson.Dumps(body, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	target := pystr.RstripChars(apiDomain, "/") + UploadInfoPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Token", token)
	req.Header.Set("X-ID", xid)
	req.Header.Set("X-Source", "web")
	if routeEnv != "" {
		req.Header.Set("X-Route-Env", routeEnv)
	}
	resp, err := pyhttpx.Do(mediaClient(), req, 15*time.Second)
	if err != nil {
		return nil, requestError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s", pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.URL, resp.Header.Get("Location")))
	}
	decoded, err := pyjson.LoadsBytes(resp.Content)
	if err != nil {
		return nil, err
	}
	result, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
	}
	if code, present := result.Get("code"); present && code != nil && !pyjson.Equal(code, int64(0)) {
		msg, ok := result.Get("msg")
		if !ok {
			msg = ""
		}
		return nil, &RuntimeError{fmt.Sprintf("genUploadInfo 失败: code=%s, msg=%s", pyrepr.Str(code), pyrepr.Str(msg))}
	}
	dataValue, _ := result.Get("data")
	if !pyrepr.Truthy(dataValue) {
		dataValue = result
	}
	data, ok := dataValue.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(dataValue))
	}
	var missing []any
	for _, f := range []string{"bucketName", "location"} {
		if v, _ := data.Get(f); !pyrepr.Truthy(v) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return nil, &RuntimeError{fmt.Sprintf("genUploadInfo 返回字段不完整: 缺少字段 %s", pyrepr.Repr(missing))}
	}
	return data, nil
}

// RuntimeError is a Python RuntimeError raised by the media helpers
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string { return e.Msg }

func credGet(c *pyjson.Object, key string) any {
	if v, ok := c.Get(key); ok {
		return v
	}
	return ""
}

func credStr(c *pyjson.Object, key string) string {
	v := credGet(c, key)
	if s, ok := v.(string); ok {
		return s
	}
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// UploadToCOS PUTs the bytes to COS signed with the temporary credentials and returns url, uuid, size and image size
func UploadToCOS(ctx context.Context, fileBytes []byte, filename, contentType string, credentials *pyjson.Object, bucket, region string) (*pyjson.Object, error) {
	secretID := credStr(credentials, "encryptTmpSecretId")
	secretKey := credStr(credentials, "encryptTmpSecretKey")
	sessionToken := credStr(credentials, "encryptToken")
	cosKey := credStr(credentials, "location")
	startTime, _ := credentials.Get("startTime")
	expiredTime, _ := credentials.Get("expiredTime")
	if secretID == "" || secretKey == "" || cosKey == "" {
		return nil, &RuntimeError{fmt.Sprintf("COS credentials 不完整: secretId=%s, secretKey=%s, location=%s",
			pyrepr.Str(secretID != ""), pyrepr.Str(secretKey != ""), pyrepr.Str(cosKey != ""))}
	}
	cosHost := bucket + ".cos.accelerate.myqcloud.com"
	encodedKey := pystr.LstripChars(pyurl.Quote(cosKey, "/"), "/")
	cosURL := "https://" + cosHost + "/" + encodedKey
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "application/octet-stream"
		if IsImage(filename, "") {
			contentType = GuessMIMEType(filename)
		}
	}
	now := big.NewInt(unixNow())
	start := now
	if pyrepr.Truthy(startTime) {
		n, ok := pyint.Instance(startTime)
		if !ok {
			return nil, fmt.Errorf("unsupported operand type(s) for +: '%s' and 'int'", pyrepr.TypeName(startTime))
		}
		start = n
	}
	expire := big.NewInt(3600)
	if pyrepr.Truthy(expiredTime) {
		n, ok := pyint.Instance(expiredTime)
		if !ok {
			return nil, fmt.Errorf("'>' not supported between instances of '%s' and 'int'", pyrepr.TypeName(expiredTime))
		}
		if n.Cmp(now) > 0 {
			expire = new(big.Int).Sub(n, now)
		}
	}
	authorization := CosSign("put", "/"+encodedKey, nil,
		[][2]string{{"host", cosHost}, {"content-type", contentType}, {"x-cos-security-token", sessionToken}},
		secretID, secretKey, start, expire)
	slog.Info(fmt.Sprintf("COS PUT: bucket=%s region=%s key=%s size=%d mime=%s", bucket, region, cosKey, len(fileBytes), contentType))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, cosURL, bytes.NewReader(fileBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-cos-security-token", sessionToken)
	resp, err := pyhttpx.Do(mediaClient(), req, 120*time.Second)
	if err != nil {
		return nil, requestError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s", pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.URL, resp.Header.Get("Location")))
	}
	resultURL := credGet(credentials, "resourceUrl")
	if !pyrepr.Truthy(resultURL) {
		resultURL = cosURL
	}
	result := pyjson.NewObject()
	result.Set("url", resultURL)
	result.Set("uuid", MD5Hex(fileBytes))
	result.Set("size", int64(len(fileBytes)))
	if strings.HasPrefix(contentType, "image/") {
		if size := ParseImageSize(fileBytes); size != nil {
			result.Set("width", size.Width)
			result.Set("height", size.Height)
		}
	}
	slog.Info(fmt.Sprintf("COS 上传成功: url=%s size=%d", pyrepr.Str(resultURL), len(fileBytes)))
	return result, nil
}

func basenameFromURL(rawURL string) string {
	u, err := pyurl.Parse(rawURL)
	if err != nil {
		return ""
	}
	return pypath.Basename(u.Path)
}

// BuildImageMsgBody builds a TIMImageElem body, uuid falling back to the filename, the URL basename, then image
func BuildImageMsgBody(rawURL, uuid, filename string, size, width, height int64, mimeType string) []any {
	id := uuid
	for _, c := range []string{filename, basenameFromURL(rawURL), "image"} {
		if id != "" {
			break
		}
		id = c
	}
	format := int64(255)
	if mimeType != "" {
		format = GetImageFormat(mimeType)
	}
	info := pyjson.NewObject()
	info.Set("type", int64(1))
	info.Set("size", size)
	info.Set("width", width)
	info.Set("height", height)
	info.Set("url", rawURL)
	content := pyjson.NewObject()
	content.Set("uuid", id)
	content.Set("image_format", format)
	content.Set("image_info_array", []any{info})
	el := pyjson.NewObject()
	el.Set("msg_type", "TIMImageElem")
	el.Set("msg_content", content)
	return []any{el}
}

// BuildFileMsgBody builds a TIMFileElem body, uuid falling back to the filename
func BuildFileMsgBody(rawURL, filename, uuid string, size int64) []any {
	if uuid == "" {
		uuid = filename
	}
	content := pyjson.NewObject()
	content.Set("uuid", uuid)
	content.Set("file_name", filename)
	content.Set("file_size", size)
	content.Set("url", rawURL)
	el := pyjson.NewObject()
	el.Set("msg_type", "TIMFileElem")
	el.Set("msg_content", content)
	return []any{el}
}
