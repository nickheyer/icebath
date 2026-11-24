package gatewayapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Room member file routes, the turn files a room home sends and the reply files it pulls
func (a *APIServerAdapter) roomFileHTTPRoutes() []httpRoute {
	return []httpRoute{
		{"POST", "/v1/room-members/files", a.handleRoomFileBegin},
		{"PUT", "/v1/room-members/files/{file_id}", a.handleRoomFileChunk},
		{"GET", "/v1/room-members/files/{file_id}", a.handleRoomFileRead},
	}
}

// Member seat scope and profile context of a verified room grant, the grant refusal when it does not verify
func (a *APIServerAdapter) roomFileScope(ctx context.Context, request *gateway.WebRequest, permission string) (context.Context, string, *gateway.WebResponse) {
	claims, err := a.roomGrantClaims(ctx, request, permission)
	if err != nil {
		return nil, "", roomGrantErrorResponse(err)
	}
	profile, _, err := localTarget(ctx, claims)
	if err != nil {
		return nil, "", roomGrantErrorResponse(err)
	}
	scope, err := gateway.PeerFileScope(claims)
	if err != nil {
		return nil, "", roomGrantErrorResponse(err)
	}
	scoped, err := profileScope(ctx, profile)
	if err != nil {
		return nil, "", roomGrantErrorResponse(err)
	}
	return scoped, scope, nil
}

// JSON answer of a file operation, a refusal carrying its status, code and the words naming the rule
func roomFileResponse(answer map[string]any, err error) (*gateway.WebResponse, error) {
	var refusal *gateway.PeerFileRefusal
	if errors.As(err, &refusal) {
		return jsonError(refusal.Msg, refusal.Code, refusal.Status), nil
	}
	if err != nil {
		return nil, err
	}
	out := jsonutil.NewObject()
	for _, key := range []string{"object", "file_id", "offset", "size", "received", "complete", "sha256", "data_base64"} {
		if value, has := answer[key]; has {
			out.Set(key, value)
		}
	}
	return jsonResponse(out, http.StatusOK, nil), nil
}

// Opens or resumes one turn file a room home declares, answering the bytes this gateway already holds
func (a *APIServerAdapter) handleRoomFileBegin(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	for _, key := range body.Keys() {
		if !strings.Contains(" name kind mime size sha256 ", " "+key+" ") {
			return jsonError("A room file accepts only name, kind, mime, size and sha256.", "invalid_attachment", http.StatusBadRequest), nil
		}
	}
	scoped, scope, refused := a.roomFileScope(ctx, request, "dispatch")
	if refused != nil {
		return refused, nil
	}
	return roomFileResponse(gateway.BeginPeerFile(scoped, gateway.HostedRoomsDefaultDBPath(ctx), scope, body))
}

// Takes one chunk of a turn file at the offset this gateway reached
func (a *APIServerAdapter) handleRoomFileChunk(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	rawOffset := body.ValueOr("offset", nil)
	_, isBool := rawOffset.(bool)
	offset, offsetErr := anyutil.ToInt(rawOffset)
	encoded, isText := body.ValueOr("data_base64", nil).(string)
	if body.Len() != 2 || isBool || offsetErr != nil || !isText {
		return jsonError("A file chunk carries only offset and data_base64.", "invalid_attachment", http.StatusBadRequest), nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return jsonError("data_base64 is not valid base64.", "invalid_attachment", http.StatusBadRequest), nil
	}
	scoped, scope, refused := a.roomFileScope(ctx, request, "dispatch")
	if refused != nil {
		return refused, nil
	}
	return roomFileResponse(gateway.WritePeerFileChunk(scoped, gateway.HostedRoomsDefaultDBPath(ctx), scope, request.MatchInfo["file_id"], offset, data))
}

// Serves one chunk of a reply file staged for the room member the grant names
func (a *APIServerAdapter) handleRoomFileRead(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	offset := int64(0)
	if values := request.Query()["offset"]; len(values) > 0 {
		parsed, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
		if err != nil {
			return jsonError("offset must be an integer.", "invalid_attachment", http.StatusBadRequest), nil
		}
		offset = parsed
	}
	scoped, scope, refused := a.roomFileScope(ctx, request, "status")
	if refused != nil {
		return refused, nil
	}
	return roomFileResponse(gateway.ReadPeerRunFile(scoped, gateway.HostedRoomsDefaultDBPath(ctx), scope, request.MatchInfo["file_id"], offset))
}

// Finished room member run status for a home that pulls files, reply files staged and MEDIA directives removed
func (a *APIServerAdapter) roomRunFiles(ctx context.Context, request *gateway.WebRequest, runID string, status *jsonutil.Object) (*jsonutil.Object, *gateway.WebResponse, error) {
	if roomGrantToken(request) == "" || request.Header.Get(gateway.PeerRoomFilesHeader) != "1" || status.ValueOr("status", nil) != "completed" {
		return status, nil, nil
	}
	output, _ := status.ValueOr("output", "").(string)
	scoped, scope, refused := a.roomFileScope(ctx, request, "status")
	if refused != nil {
		return nil, refused, nil
	}
	text, files, err := gateway.StagePeerRunFiles(scoped, gateway.HostedRoomsDefaultDBPath(ctx), scope, runID, output)
	if err != nil || len(files) == 0 && text == output {
		return status, nil, err
	}
	out := status.Clone()
	out.Set("output", text)
	listed := make([]any, len(files))
	for i, raw := range files {
		item, _ := raw.(map[string]any)
		listed[i] = jsonutil.ObjectOf("file_id", item["file_id"], "name", item["name"], "kind", item["kind"], "mime", item["mime"],
			"size", item["size"], "sha256", anyutil.String(item["sha256"]))
	}
	out.Set("files", listed)
	return out, nil, nil
}
