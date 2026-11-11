package acp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

var gatewayMediaLog = logging.GetLogger("acp_adapter.gateway_media")

// Downloads an image URL into the image cache through the SSRF guarded fetcher
var cacheImageFromURL = gateway.CacheImageFromURL

// Staged image attachment of a prompt, its cache path and sniffed type
type StagedAttachment struct {
	Path string
	Mime string
}

// Bytes of an editor's remote image URL, false when it cannot be fetched
func fetchRemoteImage(ctx context.Context, url string) ([]byte, bool, error) {
	path, err := cacheImageFromURL(ctx, url, ".img", 0)
	if err != nil {
		gatewayMediaLog.Info("ACP remote image not fetched; sent as text\n" + err.Error())
		return nil, false, nil
	}
	data, err := takeFetched(path)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Bytes of the fetched file, its cache copy removed
func takeFetched(path string) ([]byte, error) {
	defer os.Remove(path)
	return os.ReadFile(path)
}

// Text and staged image attachments of a prompt for prompt.submit, staged under the owner's profile home when given
func stageUserContent(ctx context.Context, content any, ownerHome string) (string, []StagedAttachment, error) {
	if text, ok := content.(string); ok {
		return text, []StagedAttachment{}, nil
	}
	if ownerHome != "" {
		ctx = home.WithOverride(ctx, ownerHome)
	}
	parts, _ := content.([]map[string]any)
	var texts []string
	attachments := []StagedAttachment{}
	for _, part := range parts {
		if part["type"] == "text" {
			texts = append(texts, pyStr(part["text"]))
			continue
		}
		image, _ := part["image_url"].(map[string]any)
		url := pyStr(image["url"])
		if len(attachments) >= gateway.AttachmentLimit {
			return "", nil, &gateway.GatewayClientError{Msg: "acp_content_invalid"}
		}
		var raw []byte
		if strings.HasPrefix(url, "data:") {
			_, encoded, _ := strings.Cut(url, ",")
			decoded, err := pybase64.B64DecodeValidate(encoded)
			if err != nil {
				return "", nil, &gateway.GatewayClientError{Msg: "acp_content_invalid"}
			}
			raw = decoded
		} else {
			fetched := false
			if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				var err error
				if raw, fetched, err = fetchRemoteImage(ctx, url); err != nil {
					return "", nil, err
				}
			}
			if !fetched {
				texts = append(texts, fmt.Sprintf("[Image attached: %s]", url))
				continue
			}
		}
		mime := gateway.SniffIngressImageMime(raw)
		if mime == "" {
			texts = append(texts, "[Image omitted: unsupported image format]")
			continue
		}
		path, err := mediacache.CacheImageFromBytes(ctx, home.Dir(ctx), raw, gateway.IngressImageExt[mime])
		if err != nil {
			var refused *mediacache.ValueError
			if !errors.As(err, &refused) {
				return "", nil, err
			}
			texts = append(texts, fmt.Sprintf("[Image omitted: %s]", refused.Error()))
			continue
		}
		attachments = append(attachments, StagedAttachment{Path: path, Mime: mime})
	}
	return strings.Join(texts, "\n"), attachments, nil
}

// Identity of one logical prompt as the editor sent it, taken before staging
type submitKey struct {
	SessionID string
	Digest    string
}

// Fingerprint of a prompt over its text, image data URLs and remote image URLs
func submitFingerprint(sessionID string, content any) submitKey {
	digest := sha256.New()
	add := func(kind, value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len([]rune(value))))
		digest.Write([]byte(kind))
		digest.Write(size[:])
		digest.Write([]byte(value))
	}
	if text, ok := content.(string); ok {
		add("text", text)
	} else {
		parts, _ := content.([]map[string]any)
		for _, part := range parts {
			if part["type"] == "text" {
				add("text", pyStr(part["text"]))
				continue
			}
			image, _ := part["image_url"].(map[string]any)
			add("image", pyStr(image["url"]))
		}
	}
	return submitKey{SessionID: sessionID, Digest: hex.EncodeToString(digest.Sum(nil))}
}
