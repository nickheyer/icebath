package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/state"
)

// Most inline images one API admission may commit
const apiImageLimit = 10

// Image URLs of every image_url part of a content list in order
func apiImageURLs(content []any) ([]string, error) {
	var urls []string
	for _, part := range content {
		obj, ok := part.(*pyjson.Object)
		if !ok {
			continue
		}
		if kind, _ := obj.Get("type"); kind != "image_url" {
			continue
		}
		url, err := apiPartImageURL(part)
		if err != nil {
			return nil, err
		}
		urls = append(urls, url)
	}
	return urls, nil
}

// Stages every inline data image deterministically and commits the bytes as immutable media
func CommitAPIImages(ctx context.Context, content []any) ([]any, error) {
	urls, err := apiImageURLs(content)
	if err != nil {
		return nil, err
	}
	type image struct {
		mime string
		data []byte
	}
	var images []image
	total := int64(0)
	sniffFailed := false
	for _, url := range urls {
		if _, data, ok := apiDataURLBytes(url); ok {
			mime := sniffImageMime(data)
			sniffFailed = sniffFailed || mime == ""
			images = append(images, image{mime, data})
			total += int64(len(data))
		}
	}
	limit := inboundMediaLimit(ctx)
	if len(images) > apiImageLimit || (limit != 0 && total > limit) || sniffFailed {
		return nil, runtimeRefusal("invalid_params")
	}
	cache, err := mediacache.ImageCacheDir(ctx, home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	root, err := pypath.Resolve(cache)
	if err != nil {
		return nil, err
	}
	staged := make([]string, 0, len(images))
	for _, img := range images {
		sum := sha256.Sum256(img.data)
		path := filepath.Join(root, "api_"+hex.EncodeToString(sum[:])[:32]+apiMimeExt[img.mime])
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
			if err := os.WriteFile(temporary, img.data, 0o666); err != nil {
				return nil, err
			}
			if err := os.Rename(temporary, path); err != nil {
				return nil, err
			}
		}
		staged = append(staged, path)
	}
	return CaptureNativeMedia(ctx, staged)
}

// Transient content keeping its pixels with the committed reference of each inline image appended to its text
func RestoreAPIImages(ctx context.Context, content []any, media []any) ([]any, error) {
	paths, err := restoreNativeMedia(ctx, media)
	if err != nil {
		return nil, err
	}
	urls, err := apiImageURLs(content)
	if err != nil {
		return nil, err
	}
	var hints []string
	next := 0
	for _, url := range urls {
		if _, _, ok := apiDataURLBytes(url); ok {
			if next >= len(paths) {
				return nil, &PyError{Kind: "StopIteration", Msg: ""}
			}
			hints = append(hints, "[Image attached at: "+paths[next]+"]")
			next++
			continue
		}
		hints = append(hints, "[Image attached: "+url+"]")
	}
	if len(hints) == 0 {
		return content, nil
	}
	parts := make([]any, len(content))
	var text *pyjson.Object
	for i, part := range content {
		obj, ok := part.(*pyjson.Object)
		if !ok {
			return nil, &PyError{Kind: "TypeError", Msg: "cannot convert dictionary update sequence element #0 to a sequence"}
		}
		copied := obj.Copy()
		parts[i] = copied
		if kind, _ := copied.Get("type"); kind == "text" && text == nil {
			text = copied
		}
	}
	if text == nil {
		text = pyjson.ObjectOf("type", "text", "text", "What do you see in this image?")
		parts = append([]any{text}, parts...)
	}
	current, _ := text.Get("text")
	body, ok := current.(string)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "unsupported operand type(s) for +: '" + jsonValueTypeName(current) + "' and 'str'"}
	}
	text.Set("text", body+"\n\n"+strings.Join(hints, "\n"))
	return parts, nil
}

// Original request content of a compacted settled row, a vanished image becoming a text notice
func rehydrateAPIImages(ctx context.Context, content any, media any) (any, error) {
	parts, ok := content.([]any)
	if !ok {
		return content, nil
	}
	references, _ := media.([]any)
	byDigest := map[string]any{}
	for _, reference := range references {
		obj, isObj := reference.(*pyjson.Object)
		if !isObj {
			return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(reference) + "' object is not subscriptable"}
		}
		digest, found := obj.Get("sha256")
		if !found {
			return nil, &pyKeyError{"sha256"}
		}
		if text, isText := digest.(string); isText {
			byDigest[text] = reference
		}
	}
	mimes := map[string]string{}
	for mime, ext := range apiMimeExt {
		mimes[ext] = mime
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		url, err := apiPartImageURL(part)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(url, apiRetainedPrefix) {
			out = append(out, part)
			continue
		}
		path := ""
		if reference, found := byDigest[url[len(apiRetainedPrefix):]]; found {
			restored, err := restoreNativeMedia(ctx, []any{reference})
			var refusal *state.RuntimeStoreError
			switch {
			case errors.As(err, &refusal):
			case err != nil:
				return nil, err
			default:
				path = restored[0]
			}
		}
		if path == "" {
			out = append(out, pyjson.ObjectOf("type", "text", "text", "[Image no longer retained]"))
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		mime, known := mimes[filepath.Ext(path)]
		if !known {
			mime = "image/png"
		}
		obj := part.(*pyjson.Object).Copy()
		rawImage, _ := obj.Get("image_url")
		image := rawImage.(*pyjson.Object).Copy()
		image.Set("url", "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(data))
		obj.Set("image_url", image)
		out = append(out, obj)
	}
	return out, nil
}
