package botdesktop

import (
	"bytes"
	"encoding/base64"
	"image/jpeg"

	"github.com/nickheyer/icebath/pkg/internal/pypil"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Largest width and height of a screen thumbnail
const (
	ThumbMaxW = 960
	ThumbMaxH = 600
)

// JPEG data URL of the running screen shrunk into the box, empty when no screen is up
func ThumbnailDataURL(env *tools.Env, maxW, maxH, quality int) (string, error) {
	published, err := PublishedEnv(env)
	if err != nil {
		return "", err
	}
	display := published["DISPLAY"]
	if display == "" {
		return "", nil
	}
	sandboxed, err := SandboxScreenRunning(env)
	if err != nil {
		return "", err
	}
	if sandboxed {
		sandbox, err := ownedSandboxEnv(env)
		if err != nil || sandbox == nil {
			return "", err
		}
		shot, err := GrabJPEG(sandbox, published, maxW, maxH, quality)
		if err != nil || len(shot) == 0 {
			return "", err
		}
		return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(shot), nil
	}
	if _, ok := launcherPid(env); !ok {
		return "", nil
	}
	xauthFile := published["XAUTHORITY"]
	if xauthFile == "" {
		xauthFile = pypil.XauFileName()
	}
	frame, err := pypil.GrabX11(display, xauthFile)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, pypil.ThumbnailBox(frame, maxW, maxH), &jpeg.Options{Quality: quality}); err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
