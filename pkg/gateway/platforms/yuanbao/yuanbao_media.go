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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	uploadInfoPath = "/api/resource/genUploadInfo"
	mediaMaxSizeMB = 50
)

var mimeToImageFormat = map[string]uint32{
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

var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true,
	".heic": true, ".tiff": true, ".ico": true}

// Last path element of a slash or backslash separated name
func baseName(name string) string {
	return name[strings.LastIndexAny(name, `/\`)+1:]
}

// Lowercased extension of the last path element, empty for a dot file without one
func fileExt(name string) string {
	base := strings.TrimLeft(baseName(name), ".")
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		return strings.ToLower(base[i:])
	}
	return ""
}

// MIME type of a file name by extension, octet stream when unknown
func guessMIME(name string) string {
	if m, ok := extToMIME[fileExt(name)]; ok {
		return m
	}
	return "application/octet-stream"
}

// Whether the MIME type or the extension names an image
func isImage(name, mime string) bool {
	return strings.HasPrefix(mime, "image/") || imageExts[fileExt(name)]
}

// TIM image format number of a MIME type, 255 when unknown
func imageFormat(mime string) uint32 {
	if f, ok := mimeToImageFormat[strings.ToLower(mime)]; ok {
		return f
	}
	return 255
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// Hex string of n random bytes
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Width and height from a PNG, JPEG, GIF or WebP header
func imageSize(buf []byte) (uint32, uint32, bool) {
	switch {
	case len(buf) >= 24 && bytes.Equal(buf[:4], []byte("\x89PNG")):
		return binary.BigEndian.Uint32(buf[16:20]), binary.BigEndian.Uint32(buf[20:24]), true
	case len(buf) >= 4 && buf[0] == 0xFF && buf[1] == 0xD8:
		for i := 2; i < len(buf)-9; {
			switch {
			case buf[i] != 0xFF:
				i++
			case buf[i+1] == 0xC0 || buf[i+1] == 0xC2:
				return uint32(binary.BigEndian.Uint16(buf[i+7 : i+9])), uint32(binary.BigEndian.Uint16(buf[i+5 : i+7])), true
			default:
				i += 2 + int(binary.BigEndian.Uint16(buf[i+2:i+4]))
			}
		}
	case len(buf) >= 10 && (string(buf[:6]) == "GIF87a" || string(buf[:6]) == "GIF89a"):
		return uint32(binary.LittleEndian.Uint16(buf[6:8])), uint32(binary.LittleEndian.Uint16(buf[8:10])), true
	case len(buf) >= 16 && string(buf[:4]) == "RIFF" && string(buf[8:12]) == "WEBP":
		le24 := func(b []byte) uint32 { return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 }
		switch chunk := string(buf[12:16]); {
		case chunk == "VP8 " && len(buf) >= 30 && bytes.Equal(buf[23:26], []byte{0x9d, 0x01, 0x2a}):
			return uint32(binary.LittleEndian.Uint16(buf[26:28]) & 0x3FFF), uint32(binary.LittleEndian.Uint16(buf[28:30]) & 0x3FFF), true
		case chunk == "VP8L" && len(buf) >= 25 && buf[20] == 0x2F:
			bits := binary.LittleEndian.Uint32(buf[21:25])
			return bits&0x3FFF + 1, (bits>>14)&0x3FFF + 1, true
		case chunk == "VP8X" && len(buf) >= 30:
			return le24(buf[24:27]) + 1, le24(buf[27:30]) + 1, true
		}
	}
	return 0, 0, false
}

// Body and content type of a public URL fetched behind the SSRF guard within the size cap
func downloadURL(ctx context.Context, rawURL string, maxSizeMB int64) ([]byte, string, error) {
	env := gateway.URLSafetyEnv(ctx)
	if !tools.IsSafeURL(env, rawURL) {
		return nil, "", fmt.Errorf("blocked unsafe URL (SSRF protection): %s", rawURL)
	}
	maxBytes := maxSizeMB * 1024 * 1024
	client := tools.NewSSRFSafeClient(env, 30*time.Second, true)
	if head, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil); err == nil {
		if resp, err := client.Do(head); err == nil {
			resp.Body.Close()
			if resp.ContentLength > maxBytes {
				return nil, "", fmt.Errorf("file too large: %.1f MB > %d MB", float64(resp.ContentLength)/1024/1024, maxSizeMB)
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("download of %s failed with HTTP %d", rawURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("file too large: exceeds the %d MB limit", maxSizeMB)
	}
	contentType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return data, strings.TrimSpace(contentType), nil
}

func hmacSHA1Hex(key, msg string) string {
	m := hmac.New(sha1.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// Percent encoding of every byte outside the unreserved set, slashes kept when keepSlash
func percentEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~',
			c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// q-sign-algorithm sha1 COS Authorization header over the method, path and signed headers
func cosSign(method, signPath string, headers [][2]string, secretID, secretKey string, start, expire int64) string {
	signTime := strconv.FormatInt(start, 10) + ";" + strconv.FormatInt(start+expire, 10)
	signKey := hmacSHA1Hex(secretKey, signTime)
	signed := make([][2]string, len(headers))
	for i, p := range headers {
		signed[i] = [2]string{strings.ToLower(p[0]), percentEncode(p[1], false)}
	}
	sort.Slice(signed, func(i, j int) bool {
		if signed[i][0] != signed[j][0] {
			return signed[i][0] < signed[j][0]
		}
		return signed[i][1] < signed[j][1]
	})
	kv, keys := make([]string, len(signed)), make([]string, len(signed))
	for i, p := range signed {
		kv[i], keys[i] = p[0]+"="+p[1], p[0]
	}
	headerKV, headerKeys := strings.Join(kv, "&"), strings.Join(keys, ";")
	httpString := strings.Join([]string{strings.ToLower(method), signPath, "", headerKV, ""}, "\n")
	digest := sha1.Sum([]byte(httpString))
	stringToSign := strings.Join([]string{"sha1", signTime, hex.EncodeToString(digest[:]), ""}, "\n")
	return fmt.Sprintf("q-sign-algorithm=sha1&q-ak=%s&q-sign-time=%s&q-key-time=%s&q-header-list=%s&q-url-param-list=%s&q-signature=%s",
		secretID, signTime, signTime, headerKeys, "", hmacSHA1Hex(signKey, stringToSign))
}

// Temporary COS upload credentials and target from genUploadInfo
type cosCredentials struct {
	Bucket, Region, Location, SecretID, SecretKey, SessionToken, ResourceURL string
	StartTime, ExpiredTime                                                   int64
}

// Text of a decoded JSON scalar, empty for null and containers
func jsonText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

// Integer of a decoded JSON number or numeric string, zero otherwise
func jsonInt(v any) int64 {
	text := jsonText(v)
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return int64(f)
	}
	return 0
}

// Decoded JSON object of a body with numbers kept exact
func decodeJSONObject(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("response is not a JSON object")
	}
	return out, nil
}

// COS upload credentials for one file, X-ID naming the bot id or else the app key
func getCOSCredentials(ctx context.Context, appKey, apiDomain, token, filename, botID, routeEnv string) (*cosCredentials, error) {
	payload, err := json.Marshal(struct {
		FileName  string `json:"fileName"`
		FileID    string `json:"fileId"`
		DocFrom   string `json:"docFrom"`
		DocOpenID string `json:"docOpenId"`
	}{filename, randomHex(16), "localDoc", ""})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(apiDomain, "/")+uploadInfoPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	xid := botID
	if xid == "" {
		xid = appKey
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Token", token)
	req.Header.Set("X-ID", xid)
	req.Header.Set("X-Source", "web")
	if routeEnv != "" {
		req.Header.Set("X-Route-Env", routeEnv)
	}
	resp, err := gateway.TrustEnvHTTPClient(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("genUploadInfo returned HTTP %d", resp.StatusCode)
	}
	result, err := decodeJSONObject(body)
	if err != nil {
		return nil, fmt.Errorf("genUploadInfo response: %w", err)
	}
	if code, present := result["code"]; present && code != nil && jsonText(code) != "0" {
		return nil, fmt.Errorf("genUploadInfo failed: code=%s, msg=%s", jsonText(code), jsonText(result["msg"]))
	}
	data, ok := result["data"].(map[string]any)
	if !ok || len(data) == 0 {
		data = result
	}
	creds := &cosCredentials{Bucket: jsonText(data["bucketName"]), Region: jsonText(data["region"]), Location: jsonText(data["location"]),
		SecretID: jsonText(data["encryptTmpSecretId"]), SecretKey: jsonText(data["encryptTmpSecretKey"]),
		SessionToken: jsonText(data["encryptToken"]), ResourceURL: jsonText(data["resourceUrl"]),
		StartTime: jsonInt(data["startTime"]), ExpiredTime: jsonInt(data["expiredTime"])}
	var missing []string
	if creds.Bucket == "" {
		missing = append(missing, "bucketName")
	}
	if creds.Location == "" {
		missing = append(missing, "location")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("genUploadInfo response is missing %s", strings.Join(missing, ", "))
	}
	return creds, nil
}

// Uploaded COS object with its public URL, content digest, size and image dimensions
type cosUpload struct {
	URL           string
	UUID          string
	Size          uint32
	Width, Height uint32
}

// PUTs the bytes to COS through the accelerate host signed with the temporary credentials
func uploadToCOS(ctx context.Context, data []byte, filename, contentType string, creds *cosCredentials) (*cosUpload, error) {
	if creds.SecretID == "" || creds.SecretKey == "" || creds.Location == "" {
		return nil, fmt.Errorf("incomplete COS credentials: secretId=%t, secretKey=%t, location=%t",
			creds.SecretID != "", creds.SecretKey != "", creds.Location != "")
	}
	host := creds.Bucket + ".cos.accelerate.myqcloud.com"
	encodedKey := strings.TrimLeft(percentEncode(creds.Location, true), "/")
	cosURL := "https://" + host + "/" + encodedKey
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "application/octet-stream"
		if isImage(filename, "") {
			contentType = guessMIME(filename)
		}
	}
	now := time.Now().Unix()
	start, expire := now, int64(3600)
	if creds.StartTime != 0 {
		start = creds.StartTime
	}
	if creds.ExpiredTime > now {
		expire = creds.ExpiredTime - now
	}
	authorization := cosSign("put", "/"+encodedKey,
		[][2]string{{"host", host}, {"content-type", contentType}, {"x-cos-security-token", creds.SessionToken}},
		creds.SecretID, creds.SecretKey, start, expire)
	slog.Info(fmt.Sprintf("[Yuanbao] COS PUT: bucket=%s region=%s key=%s size=%d mime=%s", creds.Bucket, creds.Region,
		creds.Location, len(data), contentType))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, cosURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-cos-security-token", creds.SessionToken)
	resp, err := gateway.TrustEnvHTTPClient(ctx, 120*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("COS upload returned HTTP %d", resp.StatusCode)
	}
	out := &cosUpload{URL: creds.ResourceURL, UUID: md5Hex(data), Size: uint32(len(data))}
	if out.URL == "" {
		out.URL = cosURL
	}
	if strings.HasPrefix(contentType, "image/") {
		out.Width, out.Height, _ = imageSize(data)
	}
	slog.Info(fmt.Sprintf("[Yuanbao] COS upload done: url=%s size=%d", out.URL, len(data)))
	return out, nil
}

// TIMImageElem of an uploaded original image
func imageElem(up *cosUpload, mime string) msgElem {
	return msgElem{Type: "TIMImageElem", Content: msgContent{UUID: up.UUID, ImageFormat: imageFormat(mime),
		Images: []imageInfo{{Type: 1, Size: up.Size, Width: up.Width, Height: up.Height, URL: up.URL}}}}
}

// TIMFileElem of an uploaded file
func fileElem(up *cosUpload, filename string) msgElem {
	return msgElem{Type: "TIMFileElem", Content: msgContent{UUID: up.UUID, FileName: filename, FileSize: up.Size, URL: up.URL}}
}

// TIMTextElem of a text
func textElem(text string) msgElem {
	return msgElem{Type: textElemType, Content: msgContent{Text: text}}
}

// Resource id query parameter of a Yuanbao resource URL, empty when absent
func parseResourceID(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	for _, key := range []string{"resourceId", "resourceid"} {
		if vs := q[key]; len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
	}
	return ""
}
