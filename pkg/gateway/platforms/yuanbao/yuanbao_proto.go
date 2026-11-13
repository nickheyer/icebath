package yuanbao

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	bizModule        = "yuanbao_openclaw_proxy"
	connModule       = "conn_access"
	instanceID       = "17"
	heartbeatRunning = 1
	heartbeatFinish  = 2
	cmdTypeRequest   = 0
	cmdTypeResponse  = 1
	cmdTypePush      = 2
	cmdTypePushAck   = 3
	cmdAuthBind      = "auth-bind"
	cmdPing          = "ping"
	cmdKickout       = "kickout"
	codeAlreadyAuth  = 41101
)

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

var seqCounter atomic.Uint32

// Next frame sequence number, wrapping at the uint32 range
func nextSeq() uint32 { return seqCounter.Add(1) - 1 }

// Protobuf encoder that omits zero values the way proto3 does
type pbWriter struct{ buf []byte }

func (w *pbWriter) key(num, wire int) {
	w.buf = binary.AppendUvarint(w.buf, uint64(num)<<3|uint64(wire))
}

func (w *pbWriter) uint(num int, v uint64) {
	if v != 0 {
		w.key(num, wireVarint)
		w.buf = binary.AppendUvarint(w.buf, v)
	}
}

func (w *pbWriter) bool(num int, v bool) {
	if v {
		w.uint(num, 1)
	}
}

func (w *pbWriter) str(num int, s string) {
	if s != "" {
		w.raw(num, []byte(s))
	}
}

func (w *pbWriter) bytes(num int, b []byte) {
	if len(b) > 0 {
		w.raw(num, b)
	}
}

// Length delimited field written even when empty
func (w *pbWriter) raw(num int, b []byte) {
	w.key(num, wireBytes)
	w.buf = binary.AppendUvarint(w.buf, uint64(len(b)))
	w.buf = append(w.buf, b...)
}

// One wire value of a parsed field
type pbValue struct {
	wire int
	num  uint64
	data []byte
}

// Parsed protobuf message keyed by field number with values in wire order
type pbFields map[int][]pbValue

// Fields of a protobuf message, an error for a truncated or unknown encoding
func parsePB(data []byte) (pbFields, error) {
	out := pbFields{}
	for pos := 0; pos < len(data); {
		tag, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			return nil, errors.New("malformed protobuf field tag")
		}
		pos += n
		v := pbValue{wire: int(tag & 7)}
		switch v.wire {
		case wireVarint:
			x, n := binary.Uvarint(data[pos:])
			if n <= 0 {
				return nil, errors.New("malformed protobuf varint")
			}
			v.num, pos = x, pos+n
		case wireBytes:
			l, n := binary.Uvarint(data[pos:])
			if n <= 0 || l > uint64(len(data)-pos-n) {
				return nil, errors.New("truncated protobuf length delimited field")
			}
			pos += n
			v.data, pos = data[pos:pos+int(l)], pos+int(l)
		case wireFixed64, wireFixed32:
			size := 8
			if v.wire == wireFixed32 {
				size = 4
			}
			if len(data)-pos < size {
				return nil, errors.New("truncated protobuf fixed field")
			}
			v.data, pos = data[pos:pos+size], pos+size
		default:
			return nil, fmt.Errorf("unknown protobuf wire type %d", v.wire)
		}
		num := int(tag >> 3)
		out[num] = append(out[num], v)
	}
	return out, nil
}

func (f pbFields) last(num, wire int) (pbValue, bool) {
	vs := f[num]
	for i := len(vs) - 1; i >= 0; i-- {
		if vs[i].wire == wire {
			return vs[i], true
		}
	}
	return pbValue{}, false
}

func (f pbFields) str(num int) string {
	if v, ok := f.last(num, wireBytes); ok {
		return strings.ToValidUTF8(string(v.data), "�")
	}
	return ""
}

func (f pbFields) bytesOf(num int) []byte {
	if v, ok := f.last(num, wireBytes); ok {
		return v.data
	}
	return nil
}

func (f pbFields) uint(num int) uint64 {
	if v, ok := f.last(num, wireVarint); ok {
		return v.num
	}
	return 0
}

func (f pbFields) int32(num int) int32 { return int32(f.uint(num)) }

func (f pbFields) repeated(num int) [][]byte {
	var out [][]byte
	for _, v := range f[num] {
		if v.wire == wireBytes {
			out = append(out, v.data)
		}
	}
	return out
}

// Envelope header of every websocket frame
type connHead struct {
	CmdType uint32
	Cmd     string
	SeqNo   uint32
	MsgID   string
	Module  string
	NeedAck bool
	Status  int32
}

func (h connHead) encode() []byte {
	w := pbWriter{}
	w.uint(1, uint64(h.CmdType))
	w.str(2, h.Cmd)
	w.uint(3, uint64(h.SeqNo))
	w.str(4, h.MsgID)
	w.str(5, h.Module)
	w.bool(6, h.NeedAck)
	w.uint(10, uint64(int64(h.Status)))
	return w.buf
}

// Frame bytes of a header and its payload
func encodeConnMsg(h connHead, data []byte) []byte {
	w := pbWriter{}
	w.raw(1, h.encode())
	w.bytes(2, data)
	return w.buf
}

// Header and payload of one websocket frame
func decodeConnMsg(frame []byte) (connHead, []byte, error) {
	f, err := parsePB(frame)
	if err != nil {
		return connHead{}, nil, err
	}
	hf, err := parsePB(f.bytesOf(1))
	if err != nil {
		return connHead{}, nil, err
	}
	h := connHead{CmdType: uint32(hf.uint(1)), Cmd: hf.str(2), SeqNo: uint32(hf.uint(3)), MsgID: hf.str(4),
		Module: hf.str(5), NeedAck: hf.uint(6) != 0, Status: hf.int32(10)}
	return h, f.bytesOf(2), nil
}

// Request frame stamped with a fresh sequence number
func connRequest(cmdType uint32, cmd, msgID, module string, data []byte) []byte {
	return encodeConnMsg(connHead{CmdType: cmdType, Cmd: cmd, SeqNo: nextSeq(), MsgID: msgID, Module: module}, data)
}

// Business request frame and its request id, drawn from prefix and the sequence when msgID is empty
func bizRequest(method, prefix, msgID string, body []byte) ([]byte, string) {
	if msgID == "" {
		msgID = prefix + "_" + strconv.FormatUint(uint64(nextSeq()), 10)
	}
	return connRequest(cmdTypeRequest, method, msgID, bizModule, body), msgID
}

// One key value entry of a message content ext map in wire order
type extEntry struct{ Key, Value string }

// Image variant of a TIMImageElem
type imageInfo struct {
	Type, Size, Width, Height uint32
	URL                       string
}

// Content fields of a TIM message element
type msgContent struct {
	Text, UUID, Data, URL, FileName string
	ImageFormat, Index, FileSize    uint32
	Images                          []imageInfo
	ExtMap                          []extEntry
}

// One TIM message element
type msgElem struct {
	Type    string
	Content msgContent
}

func (c msgContent) encode() []byte {
	w := pbWriter{}
	w.str(1, c.Text)
	w.str(2, c.UUID)
	w.uint(3, uint64(c.ImageFormat))
	w.str(4, c.Data)
	for _, img := range c.Images {
		iw := pbWriter{}
		iw.uint(1, uint64(img.Type))
		iw.uint(2, uint64(img.Size))
		iw.uint(3, uint64(img.Width))
		iw.uint(4, uint64(img.Height))
		iw.str(5, img.URL)
		w.raw(8, iw.buf)
	}
	w.uint(9, uint64(c.Index))
	w.str(10, c.URL)
	w.uint(11, uint64(c.FileSize))
	w.str(12, c.FileName)
	for _, e := range c.ExtMap {
		ew := pbWriter{}
		ew.str(1, e.Key)
		ew.str(2, e.Value)
		w.raw(999, ew.buf)
	}
	return w.buf
}

func decodeMsgContent(data []byte) (msgContent, error) {
	f, err := parsePB(data)
	if err != nil {
		return msgContent{}, err
	}
	c := msgContent{Text: f.str(1), UUID: f.str(2), ImageFormat: uint32(f.uint(3)), Data: f.str(4), Index: uint32(f.uint(9)),
		URL: f.str(10), FileSize: uint32(f.uint(11)), FileName: f.str(12)}
	for _, raw := range f.repeated(8) {
		inf, err := parsePB(raw)
		if err != nil {
			return msgContent{}, err
		}
		img := imageInfo{Type: uint32(inf.uint(1)), Size: uint32(inf.uint(2)), Width: uint32(inf.uint(3)),
			Height: uint32(inf.uint(4)), URL: inf.str(5)}
		if img != (imageInfo{}) {
			c.Images = append(c.Images, img)
		}
	}
	for _, raw := range f.repeated(999) {
		ef, err := parsePB(raw)
		if err != nil {
			return msgContent{}, err
		}
		if k := ef.str(1); k != "" {
			c.ExtMap = append(c.ExtMap, extEntry{k, ef.str(2)})
		}
	}
	return c, nil
}

func (e msgElem) encode() []byte {
	w := pbWriter{}
	w.str(1, e.Type)
	w.bytes(2, e.Content.encode())
	return w.buf
}

func decodeMsgElem(data []byte) (msgElem, error) {
	f, err := parsePB(data)
	if err != nil {
		return msgElem{}, err
	}
	e := msgElem{Type: f.str(1)}
	if raw := f.bytesOf(2); len(raw) > 0 {
		if e.Content, err = decodeMsgContent(raw); err != nil {
			return msgElem{}, err
		}
	}
	return e, nil
}

// Recalled message reference of a recall callback
type recallRef struct {
	MsgSeq uint64
	MsgID  string
}

// Inbound message push or callback delivered by the Yuanbao gateway
type inboundPush struct {
	CallbackCommand, FromAccount, SenderNickname, GroupCode, GroupName string
	MsgID, CloudCustomData, BotOwnerID                                 string
	MsgSeq                                                             uint64
	MsgBody                                                            []msgElem
	RecallList                                                         []recallRef
}

// Whether the push names a callback, a sender or a message body
func (p *inboundPush) valid() bool {
	return p.CallbackCommand != "" || p.FromAccount != "" || len(p.MsgBody) > 0
}

// InboundMessagePush payload, an error for a malformed encoding
func decodeInboundPush(data []byte) (*inboundPush, error) {
	f, err := parsePB(data)
	if err != nil {
		return nil, err
	}
	p := &inboundPush{CallbackCommand: f.str(1), FromAccount: f.str(2), SenderNickname: f.str(4), GroupCode: f.str(6),
		GroupName: f.str(7), MsgSeq: f.uint(8), MsgID: f.str(12), CloudCustomData: f.str(14), BotOwnerID: f.str(16)}
	if p.MsgID == "" {
		p.MsgID = f.str(11)
	}
	for _, raw := range f.repeated(13) {
		e, err := decodeMsgElem(raw)
		if err != nil {
			return nil, err
		}
		p.MsgBody = append(p.MsgBody, e)
	}
	for _, raw := range f.repeated(17) {
		rf, err := parsePB(raw)
		if err != nil {
			return nil, err
		}
		p.RecallList = append(p.RecallList, recallRef{MsgSeq: rf.uint(1), MsgID: rf.str(2)})
	}
	return p, nil
}

// Media item of a forwarded chat record
type forwardMedia struct{ Type, URL, FileName, MediaID string }

// Content item of a forwarded chat record, type 1 text, 2 media, 3 nested forward
type forwardContent struct {
	Type  uint64
	Text  string
	Media []forwardMedia
}

// One message of a forwarded chat record
type forwardMsg struct {
	Sender, PlainText string
	Contents          []forwardContent
}

// WeChat chat record forward carried base64 encoded in a custom element ext map
type forwardData struct {
	SubType uint64
	Msgs    []forwardMsg
}

// ForwardMsgData payload, an error for a malformed encoding
func decodeForwardData(data []byte) (*forwardData, error) {
	f, err := parsePB(data)
	if err != nil {
		return nil, err
	}
	out := &forwardData{SubType: f.uint(1)}
	for _, raw := range f.repeated(5) {
		mf, err := parsePB(raw)
		if err != nil {
			return nil, err
		}
		m := forwardMsg{Sender: mf.str(1), PlainText: mf.str(3)}
		for _, craw := range mf.repeated(4) {
			cf, err := parsePB(craw)
			if err != nil {
				return nil, err
			}
			c := forwardContent{Type: cf.uint(1), Text: cf.str(2)}
			for _, mraw := range cf.repeated(3) {
				df, err := parsePB(mraw)
				if err != nil {
					return nil, err
				}
				c.Media = append(c.Media, forwardMedia{Type: df.str(1), URL: df.str(2), FileName: df.str(4), MediaID: df.str(15)})
			}
			m.Contents = append(m.Contents, c)
		}
		out.Msgs = append(out.Msgs, m)
	}
	return out, nil
}

// Repeated message body field of a send request
func encodeBody(w *pbWriter, num int, body []msgElem) {
	for _, e := range body {
		w.bytes(num, e.encode())
	}
}

// SendC2CMessageReq frame and request id, group code marking a DM that started in a group
func encodeSendC2C(toAccount, fromAccount, groupCode string, body []msgElem) ([]byte, string) {
	reqID := "c2c_" + strconv.FormatUint(uint64(nextSeq()), 10)
	w := pbWriter{}
	w.str(1, reqID)
	w.str(2, toAccount)
	w.str(3, fromAccount)
	encodeBody(&w, 5, body)
	w.str(6, groupCode)
	return bizRequest("send_c2c_message", "c2c", reqID, w.buf)
}

// SendGroupMessageReq frame and request id, refMsgID quoting the replied message
func encodeSendGroup(groupCode, fromAccount, refMsgID string, body []msgElem) ([]byte, string) {
	reqID := "grp_" + strconv.FormatUint(uint64(nextSeq()), 10)
	w := pbWriter{}
	w.str(1, reqID)
	w.str(2, groupCode)
	w.str(3, fromAccount)
	encodeBody(&w, 6, body)
	w.str(7, refMsgID)
	return bizRequest("send_group_message", "grp", reqID, w.buf)
}

// AuthBindReq frame with the auth info, device info and route env
func encodeAuthBind(uid, source, token, msgID, appVersion, osName, botVersion, routeEnv string) []byte {
	auth := pbWriter{}
	auth.str(1, uid)
	auth.str(2, source)
	auth.str(3, token)
	dev := pbWriter{}
	dev.str(1, appVersion)
	dev.str(2, osName)
	dev.str(10, instanceID)
	dev.str(24, botVersion)
	req := pbWriter{}
	req.str(1, "ybBot")
	req.bytes(2, auth.buf)
	req.bytes(3, dev.buf)
	req.str(5, routeEnv)
	return connRequest(cmdTypeRequest, cmdAuthBind, msgID, connModule, req.buf)
}

// Empty PingReq frame
func encodePing(msgID string) []byte {
	return connRequest(cmdTypeRequest, cmdPing, msgID, connModule, nil)
}

// Push acknowledgement echoing the cmd, msg id and module of the push
func encodePushAck(h connHead) []byte {
	return connRequest(cmdTypePushAck, h.Cmd, h.MsgID, h.Module, nil)
}

// SendPrivateHeartbeatReq frame with a RUNNING or FINISH state
func encodePrivateHeartbeat(fromAccount, toAccount string, state uint64) []byte {
	w := pbWriter{}
	w.str(1, fromAccount)
	w.str(2, toAccount)
	w.uint(3, state)
	frame, _ := bizRequest("send_private_heartbeat", "hb_priv", "", w.buf)
	return frame
}

// SendGroupHeartbeatReq frame stamped with the current time in milliseconds
func encodeGroupHeartbeat(fromAccount, groupCode string, state uint64) []byte {
	w := pbWriter{}
	w.str(1, fromAccount)
	w.str(3, groupCode)
	w.uint(4, uint64(time.Now().UnixMilli()))
	w.uint(5, state)
	frame, _ := bizRequest("send_group_heartbeat", "hb_grp", "", w.buf)
	return frame
}

// QueryGroupInfoReq frame and request id
func encodeQueryGroupInfo(groupCode string) ([]byte, string) {
	w := pbWriter{}
	w.str(1, groupCode)
	return bizRequest("query_group_info", "qgi", "", w.buf)
}

// GetGroupMemberListReq frame and request id
func encodeGetGroupMemberList(groupCode string) ([]byte, string) {
	w := pbWriter{}
	w.str(1, groupCode)
	return bizRequest("get_group_member_list", "gml", "", w.buf)
}

// Code and message every business response opens with
type rspStatus struct {
	Code    int32
	Message string
}

// Status fields of a send or heartbeat response
func decodeRspStatus(data []byte) (rspStatus, error) {
	f, err := parsePB(data)
	if err != nil {
		return rspStatus{}, err
	}
	return rspStatus{Code: f.int32(1), Message: f.str(2)}, nil
}

// AuthBindRsp status and connect id
func decodeAuthBindRsp(data []byte) (rspStatus, string, error) {
	f, err := parsePB(data)
	if err != nil {
		return rspStatus{}, "", err
	}
	return rspStatus{Code: f.int32(1), Message: f.str(2)}, f.str(3), nil
}

// Group profile of a QueryGroupInfoRsp
type groupInfo struct {
	Name, OwnerID, OwnerNickname string
	MemberCount                  int64
}

// QueryGroupInfoRsp status and group profile
func decodeQueryGroupInfoRsp(data []byte) (rspStatus, groupInfo, error) {
	f, err := parsePB(data)
	if err != nil {
		return rspStatus{}, groupInfo{}, err
	}
	gf, err := parsePB(f.bytesOf(3))
	if err != nil {
		return rspStatus{}, groupInfo{}, err
	}
	return rspStatus{Code: f.int32(1), Message: f.str(2)},
		groupInfo{Name: gf.str(1), OwnerID: gf.str(2), OwnerNickname: gf.str(3), MemberCount: int64(gf.int32(4))}, nil
}

// One member of a GetGroupMemberListRsp, user type 1 user, 2 Yuanbao AI, 3 bot
type groupMember struct {
	UserID, Nickname string
	UserType         int64
}

// GetGroupMemberListRsp status and members
func decodeGroupMemberListRsp(data []byte) (rspStatus, []groupMember, error) {
	f, err := parsePB(data)
	if err != nil {
		return rspStatus{}, nil, err
	}
	var members []groupMember
	for _, raw := range f.repeated(3) {
		mf, err := parsePB(raw)
		if err != nil {
			return rspStatus{}, nil, err
		}
		members = append(members, groupMember{UserID: mf.str(1), Nickname: mf.str(2), UserType: int64(mf.int32(3))})
	}
	return rspStatus{Code: f.int32(1), Message: f.str(2)}, members, nil
}

// Kickout notice the gateway pushes before dropping the connection
type kickoutMsg struct {
	Status          int32
	Reason          string
	OtherDeviceName string
}

func decodeKickout(data []byte) (kickoutMsg, error) {
	f, err := parsePB(data)
	if err != nil {
		return kickoutMsg{}, err
	}
	return kickoutMsg{Status: f.int32(1), Reason: f.str(2), OtherDeviceName: f.str(3)}, nil
}
