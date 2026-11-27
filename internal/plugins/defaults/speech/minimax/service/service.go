// MiniMax voices, of its global service or its China one
package service

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/minimax/config"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Endpoints of each region and the hosts MiniMax serves each from
var (
	endpoints = map[string]string{"global": config.GlobalBase, "cn": config.ChinaBase}
	official  = map[string]map[string]bool{"global": strs.Set("api.minimax.io", "api.minimax.chat"), "cn": strs.Set("api.minimaxi.com")}
)

func Register(registry *speech.Registry) {
	registry.Register(speech.Services{Speakers: []speech.Voice{{
		Name: config.Name, Title: "MiniMax", Label: "MiniMax TTS", About: "Voices with emotion and pitch control, needs a MiniMax API key",
		Ready:      func(job *speech.Job) bool { _, err := route(job); return err == nil },
		Limit:      func(*speech.Job) int { return config.DefaultLimit },
		Synthesize: synthesize,
		Opus:       speech.TranscodedOpus,
		Setup:      speech.Setup{Keys: []string{config.GlobalKeyEnv, config.ChinaKeyEnv}, SignIn: "minimax", Link: "https://www.minimax.io/platform"},
	}}})
}

// Region, endpoint and key chosen together: the region set, else global, or China when only a China key is set
type runtime struct {
	region, endpoint, key string
}

func route(job *speech.Job) (runtime, error) {
	keys := map[string][2]string{
		"global": {config.GlobalKeyEnv, job.Key(config.GlobalKeyEnv, "minimax")},
		"cn":     {config.ChinaKeyEnv, job.Key(config.ChinaKeyEnv, "minimax-cn")},
	}
	region := strings.ToLower(job.Text(config.Region, ""))
	if region == "" {
		region = "global"
		if keys["cn"][1] != "" && keys["global"][1] == "" {
			region = "cn"
		}
	}
	source, key := keys[region][0], keys[region][1]
	if key == "" {
		return runtime{}, speech.Misconfigured(fmt.Sprintf("%s not set for MiniMax TTS region %s", source, region))
	}
	endpoint := job.Text(config.BaseURL, endpoints[region])
	other := "global"
	if region == "global" {
		other = "cn"
	}
	if parsed, err := url.Parse(endpoint); err == nil && official[other][strings.ToLower(parsed.Hostname())] {
		return runtime{}, speech.Misconfigured(fmt.Sprintf("tts.minimax.base_url points to the %s MiniMax endpoint but region is %s", other, region))
	}
	return runtime{region: region, endpoint: endpoint, key: key}, nil
}

// Failure of a reply whose base_resp status is not 0
func refused(result map[string]any) error {
	base, _ := result["base_resp"].(map[string]any)
	status, present := base["status_code"]
	if !present {
		status = -1
	}
	if code, ok := status.(float64); ok && code == 0 {
		return nil
	}
	message := "unknown error"
	if text, ok := base["status_msg"]; ok {
		message = speech.Text(text)
	}
	return fmt.Errorf("MiniMax TTS API error (code %s): %s", speech.Text(status), message)
}

// Speech through t2a_v2, which answers hex audio in JSON, or a legacy endpoint answering audio; told apart by the address
func synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	rt, err := route(job)
	if err != nil {
		return "", err
	}
	model, voice := job.Text(config.Model, config.DefaultModel), job.Text(config.VoiceID, config.DefaultVoice)
	speed, _ := job.Speed(request, config.Speed)
	address := rt.endpoint
	if group := strs.Filled(job.Text(config.GroupID, ""), job.Getenv(config.GroupEnv)); group != "" && !strings.Contains(address, "GroupId=") {
		separator := "?"
		if strings.Contains(address, "?") {
			separator = "&"
		}
		address += separator + "GroupId=" + group
	}
	modern := strings.Contains(address, "t2a_v2")
	payload := map[string]any{"model": model, "text": request.Text, "voice_id": voice}
	if modern {
		payload = map[string]any{"model": model, "text": request.Text,
			"voice_setting": map[string]any{"voice_id": voice, "speed": speed, "vol": job.Float(config.Volume), "pitch": job.Config.GetInt(config.Pitch),
				"emotion": job.Text(config.Emotion, "neutral")},
			"audio_setting": map[string]any{"sample_rate": job.Config.GetInt(config.SampleRate), "bitrate": job.Config.GetInt(config.Bitrate), "format": "mp3", "channel": 1}}
	}
	response, err := speech.PostJSON(ctx, address, payload, map[string]string{"Authorization": "Bearer " + rt.key})
	if err != nil {
		return "", err
	}
	if modern {
		if err := speech.Failed(response); err != nil {
			return "", err
		}
		result, err := speech.Object(response, "MiniMax TTS")
		if err != nil {
			return "", err
		}
		if err := refused(result); err != nil {
			return "", err
		}
		data, _ := result["data"].(map[string]any)
		encoded, _ := data["audio"].(string)
		if encoded == "" {
			return "", errors.New("MiniMax TTS returned empty audio data")
		}
		audio, err := hex.DecodeString(encoded)
		if err != nil {
			return "", speech.Misconfigured(err.Error())
		}
		return request.Path, os.WriteFile(request.Path, audio, 0o666)
	}
	kind := response.Header.Get("Content-Type")
	if strings.Contains(kind, "audio/") {
		return speech.Save(response, request.Path, "MiniMax TTS")
	}
	raw, err := speech.Body(response, "MiniMax TTS", 0)
	if err != nil {
		return "", err
	}
	var decoded map[string]any
	if len(raw) > 0 {
		var parsed any
		if !utf8.Valid(raw) || json.Unmarshal(raw, &parsed) != nil {
			return "", unexpected(response, kind, len(raw))
		}
		if decoded, _ = parsed.(map[string]any); decoded == nil {
			return "", unexpected(response, kind, len(raw))
		}
	}
	if err := refused(decoded); err != nil {
		return "", err
	}
	return "", errors.New("MiniMax TTS returned no audio data")
}

// Failure of a reply that is neither audio nor a JSON object: its status when it failed, else what it held
func unexpected(response *http.Response, kind string, size int) error {
	if response.StatusCode >= 400 {
		reason := strings.TrimSpace(strings.TrimPrefix(response.Status, strconv.Itoa(response.StatusCode)))
		return &speech.StatusError{Status: response.StatusCode, Reason: reason, URL: response.Request.URL.String()}
	}
	return fmt.Errorf("MiniMax TTS returned unexpected Content-Type '%s' (%d bytes)", kind, size)
}
