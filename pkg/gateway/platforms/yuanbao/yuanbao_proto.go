package yuanbao

import (
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	bizPkg             = "yuanbao_openclaw_proxy"
	IcebathInstanceID  = 17
	WSHeartbeatRunning = 1
	WSHeartbeatFinish  = 2
	seqMax             = 1<<32 - 1
	WTVarint           = 0
	WT64Bit            = 1
	WTLen              = 2
	WT32Bit            = 5
)

var (
	PBMsgTypes  = map[string]string{}
	CmdType     = map[string]int64{"Request": 0, "Response": 1, "Push": 2, "PushAck": 3}
	Cmd         = map[string]string{"AuthBind": "auth-bind", "Ping": "ping", "Kickout": "kickout", "UpdateMeta": "update-meta"}
	Module      = map[string]string{"ConnAccess": "conn_access"}
	BizServices = map[string]string{"InboundMessagePush": bizPkg + ".InboundMessagePush"}
	fixedSize   = map[int64]int{WT64Bit: 8, WT32Bit: 4}
)

func init() {
	for _, n := range []string{"ConnMsg", "AuthBindReq", "AuthBindRsp", "PingReq", "PingRsp", "KickoutMsg", "DirectedPush", "PushMsg"} {
		PBMsgTypes[n] = "trpc.yuanbao.conn_common." + n
	}
	for _, m := range []string{"SendC2CMessage", "SendGroupMessage", "QueryGroupInfo", "GetGroupMemberList", "SendPrivateHeartbeat", "SendGroupHeartbeat"} {
		for _, k := range []string{"Req", "Rsp"} {
			BizServices[m+k] = bizPkg + "." + m + k
		}
	}
}

var (
	seqMu      sync.Mutex
	seqCounter int64
)

// NextSeqNo returns the next uint32 sequence number, wrapping to zero, safe across goroutines
func NextSeqNo() int64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	val := seqCounter
	seqCounter = (seqCounter + 1) & seqMax
	return val
}

var mask64 = new(big.Int).SetUint64(^uint64(0))

// EncodeVarint writes a protobuf varint, negatives as 64 bit two's complement
func EncodeVarint(value *big.Int) []byte {
	v := new(big.Int).Set(value)
	if v.Sign() < 0 {
		v.And(v, mask64)
	}
	var out []byte
	for {
		bits := byte(new(big.Int).And(v, big.NewInt(0x7F)).Int64())
		v.Rsh(v, 7)
		if v.Sign() == 0 {
			return append(out, bits)
		}
		out = append(out, bits|0x80)
	}
}

func varint64(n int64) []byte { return EncodeVarint(big.NewInt(n)) }

// DecodeVarint reads a varint at pos as int64 or *big.Int and returns the new position
func DecodeVarint(data []byte, pos int) (any, int, error) {
	result, shift := new(big.Int), uint(0)
	for pos < len(data) {
		b := data[pos]
		pos++
		result.Or(result, new(big.Int).Lsh(big.NewInt(int64(b&0x7F)), shift))
		shift += 7
		if b&0x80 == 0 {
			break
		}
		if shift >= 64 {
			return nil, pos, fmt.Errorf("varint too long")
		}
	}
	return pyint.Value(result), pos, nil
}

// EncodeField prefixes a value with its field tag
func EncodeField(fieldNumber int64, wireType int64, value []byte) []byte {
	return append(varint64(fieldNumber<<3|wireType), value...)
}

// EncodeMessage length prefixes bytes or a nested message
func EncodeMessage(b []byte) []byte { return append(varint64(int64(len(b))), b...) }

// EncodeString length prefixes the UTF-8 bytes of a string
func EncodeString(s string) ([]byte, error) {
	if err := pystr.StrictUTF8EncodeError(s); err != nil {
		return nil, err
	}
	return EncodeMessage([]byte(s)), nil
}

// Field is one parsed field, Value an int64 or *big.Int for varints and bytes otherwise
type Field struct {
	Number   any
	WireType int64
	Value    any
}

func asInt64(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}

// ParseFields splits a protobuf message into its fields in wire order
func ParseFields(data []byte) ([]Field, error) {
	var fields []Field
	pos := 0
	for pos < len(data) {
		tagAny, next, err := DecodeVarint(data, pos)
		if err != nil {
			return nil, err
		}
		pos = next
		tag, _ := pyint.Instance(tagAny)
		wireType := new(big.Int).And(tag, big.NewInt(7)).Int64()
		number := pyint.Value(new(big.Int).Rsh(tag, 3))
		var val any
		if wireType == WTVarint {
			val, pos, err = DecodeVarint(data, pos)
			if err != nil {
				return nil, err
			}
		} else {
			var length int
			if wireType == WTLen {
				lv, next, err := DecodeVarint(data, pos)
				if err != nil {
					return nil, err
				}
				pos = next
				l, ok := asInt64(lv)
				if !ok || l > int64(len(data)) {
					l = int64(len(data))
				}
				length = int(l)
			} else if n, ok := fixedSize[wireType]; ok {
				length = n
			} else {
				return nil, fmt.Errorf("unknown wire type %d at pos %d", wireType, pos-1)
			}
			end := min(pos+length, len(data))
			val = append([]byte{}, data[min(pos, len(data)):end]...)
			pos += length
		}
		fields = append(fields, Field{number, wireType, val})
	}
	return fields, nil
}

type fieldEntry struct {
	wireType int64
	value    any
}

// FieldDict maps a field number to its entries in wire order, repeated fields holding several
type FieldDict map[string][]fieldEntry

func fieldKey(n any) string { return pyrepr.Str(n) }

// FieldsToDict groups parsed fields by field number
func FieldsToDict(fields []Field) FieldDict {
	d := FieldDict{}
	for _, f := range fields {
		k := fieldKey(f.Number)
		d[k] = append(d[k], fieldEntry{f.WireType, f.Value})
	}
	return d
}

func parseDict(data []byte) (FieldDict, error) {
	fields, err := ParseFields(data)
	if err != nil {
		return nil, err
	}
	return FieldsToDict(fields), nil
}

func (d FieldDict) first(fn int64, wt int64) any {
	entries := d[fieldKey(int64(fn))]
	if len(entries) > 0 && entries[0].wireType == wt {
		return entries[0].value
	}
	return nil
}

// GetString decodes the first length delimited field with replacement, empty when absent
func (d FieldDict) GetString(fn int64) string {
	if b, ok := d.first(fn, WTLen).([]byte); ok {
		return pystr.DecodeUTF8Replace(b)
	}
	return ""
}

// GetVarint returns the first varint field as int64 or *big.Int, zero when absent
func (d FieldDict) GetVarint(fn int64) any {
	if v := d.first(fn, WTVarint); v != nil {
		return v
	}
	return int64(0)
}

// GetBytes returns the first length delimited field, empty when absent
func (d FieldDict) GetBytes(fn int64) []byte {
	if b, ok := d.first(fn, WTLen).([]byte); ok {
		return b
	}
	return []byte{}
}

// GetRepeatedBytes returns every length delimited entry of a field
func (d FieldDict) GetRepeatedBytes(fn int64) [][]byte {
	var out [][]byte
	for _, e := range d[fieldKey(int64(fn))] {
		if e.wireType == WTLen {
			out = append(out, e.value.([]byte))
		}
	}
	return out
}

func (d FieldDict) parseRepeated(fn int64) ([]FieldDict, error) {
	var out []FieldDict
	for _, b := range d.GetRepeatedBytes(fn) {
		fd, err := parseDict(b)
		if err != nil {
			return nil, err
		}
		out = append(out, fd)
	}
	return out, nil
}

// Sticky error protobuf writer
type pbuf struct {
	b   []byte
	err error
}

func (p *pbuf) s(fn int64, s string) {
	if p.err != nil {
		return
	}
	enc, err := EncodeString(s)
	if err != nil {
		p.err = err
		return
	}
	p.b = append(p.b, EncodeField(fn, WTLen, enc)...)
}

func (p *pbuf) v(fn int64, n *big.Int) {
	if p.err == nil {
		p.b = append(p.b, EncodeField(fn, WTVarint, EncodeVarint(n))...)
	}
}

func (p *pbuf) m(fn int64, b []byte) {
	if p.err == nil {
		p.b = append(p.b, EncodeField(fn, WTLen, EncodeMessage(b))...)
	}
}

func (p *pbuf) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

type part struct {
	fn   int64
	kind byte
	val  any
}

func typeErr(op string, v any) error {
	return fmt.Errorf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), op)
}

func intOf(v any) (*big.Int, error) {
	if n, ok := pyint.Instance(v); ok {
		return n, nil
	}
	return nil, fmt.Errorf("'<' not supported between instances of '%s' and 'int'", pyrepr.TypeName(v))
}

// Encodes the parts whose value is set, S always and n whenever not None
func encodeParts(parts []part) ([]byte, error) {
	p := &pbuf{}
	for _, pt := range parts {
		if !(pt.kind == 'S' || (pt.kind == 'n' && pt.val != nil) || (pt.kind != 'n' && truthyNum(pt.val))) {
			continue
		}
		switch pt.kind {
		case 'S', 's':
			s, ok := pt.val.(string)
			if !ok {
				p.fail(typeErr("encode", pt.val))
				continue
			}
			p.s(pt.fn, s)
		case 'v', 'n':
			n, err := intOf(pt.val)
			if err != nil {
				p.fail(err)
				continue
			}
			p.v(pt.fn, n)
		case 'm':
			p.m(pt.fn, pt.val.([]byte))
		case 'b':
			body, ok := pt.val.([]any)
			if !ok {
				p.fail(fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(pt.val)))
				continue
			}
			for _, el := range body {
				enc, err := EncodeMsgBodyElement(el)
				if err != nil {
					p.fail(err)
					break
				}
				p.m(pt.fn, enc)
			}
		case 't':
			s, ok := pt.val.(string)
			if !ok {
				p.fail(typeErr("encode", pt.val))
				continue
			}
			inner := &pbuf{}
			inner.s(1, s)
			p.fail(inner.err)
			p.m(pt.fn, inner.b)
		}
	}
	return p.b, p.err
}

type specField struct {
	fn   int64
	key  string
	kind byte
}

// Dict view over a decoded JSON object or a Go map, ok false for any other value
func asDict(v any) (map[string]any, bool) {
	switch d := v.(type) {
	case *pyjson.Object:
		return d.Vals, true
	case map[string]any:
		return d, true
	}
	return nil, false
}

// Keys of a dict in iteration order, Go maps in sorted order
func dictKeys(v any) []string {
	if o, ok := v.(*pyjson.Object); ok {
		return o.Keys
	}
	m, _ := v.(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func encodeSpec(objValue any, spec []specField) ([]byte, error) {
	obj, ok := asDict(objValue)
	if !ok {
		return nil, typeErr("get", objValue)
	}
	p := &pbuf{}
	for _, f := range spec {
		v, present := obj[f.key]
		if !present {
			v = int64(0)
			if f.kind == 's' || f.kind == 'r' {
				v = ""
			}
		}
		if !truthyNum(v) {
			continue
		}
		switch f.kind {
		case 's':
			p.s(f.fn, pyrepr.Str(v))
		case 'r':
			s, ok := v.(string)
			if !ok {
				p.fail(typeErr("encode", v))
				continue
			}
			p.s(f.fn, s)
		default:
			n, err := pyint.Of(v)
			if err != nil {
				p.fail(err)
				continue
			}
			p.v(f.fn, n)
		}
	}
	return p.b, p.err
}

// Python truth value with arbitrary precision ints counted as numbers
func truthyNum(v any) bool {
	if n, ok := v.(*big.Int); ok {
		return n.Sign() != 0
	}
	return pyrepr.Truthy(v)
}

func decodeSpec(fd FieldDict, spec []specField) *pyjson.Object {
	out := pyjson.NewObject()
	for _, f := range spec {
		var v any
		if f.kind == 's' || f.kind == 'r' {
			v = fd.GetString(f.fn)
		} else {
			v = fd.GetVarint(f.fn)
		}
		if truthyNum(v) {
			out.Set(f.key, v)
		}
	}
	return out
}

func encodeHead(cmdType int64, cmd string, seqNo int64, msgID, module string, needAck bool, status *big.Int) ([]byte, error) {
	ack := int64(0)
	if needAck {
		ack = 1
	}
	return encodeParts([]part{{1, 'v', cmdType}, {2, 's', cmd}, {3, 'v', seqNo}, {4, 's', msgID}, {5, 's', module},
		{6, 'v', ack}, {10, 'v', new(big.Int).And(status, mask64)}})
}

func decodeHead(data []byte) (*pyjson.Object, error) {
	fd, err := parseDict(data)
	if err != nil {
		return nil, err
	}
	head := pyjson.NewObject()
	head.Set("cmd_type", fd.GetVarint(1))
	head.Set("cmd", fd.GetString(2))
	head.Set("seq_no", fd.GetVarint(3))
	head.Set("msg_id", fd.GetString(4))
	head.Set("module", fd.GetString(5))
	head.Set("need_ack", truthyNum(fd.GetVarint(6)))
	head.Set("status", fd.GetVarint(10))
	return head, nil
}

// EncodeConnMsgFull encodes a ConnMsg with every head field and the optional payload
func EncodeConnMsgFull(cmdType int64, cmd string, seqNo int64, msgID, module string, data []byte, needAck bool) ([]byte, error) {
	head, err := encodeHead(cmdType, cmd, seqNo, msgID, module, needAck, big.NewInt(0))
	if err != nil {
		return nil, err
	}
	p := &pbuf{}
	p.m(1, head)
	if len(data) > 0 {
		p.m(2, data)
	}
	return p.b, nil
}

// EncodeConnMsg encodes a ConnMsg carrying only cmd_type, seq_no and the payload
func EncodeConnMsg(msgType, seqNo int64, data []byte) []byte {
	out, _ := EncodeConnMsgFull(msgType, "", seqNo, "", "", data, false)
	return out
}

// DecodeConnMsg decodes a ConnMsg into msg_type, seq_no, data and the full head
func DecodeConnMsg(data []byte) (*pyjson.Object, error) {
	fd, err := parseDict(data)
	if err != nil {
		return nil, err
	}
	head, err := decodeHead(fd.GetBytes(1))
	if err != nil {
		return nil, err
	}
	out := pyjson.NewObject()
	out.Set("msg_type", head.Vals["cmd_type"])
	out.Set("seq_no", head.Vals["seq_no"])
	out.Set("data", fd.GetBytes(2))
	out.Set("head", head)
	return out, nil
}

func connRequest(cmdType int64, cmd, msgID, module string, data []byte) ([]byte, error) {
	return EncodeConnMsgFull(cmdType, cmd, NextSeqNo(), msgID, module, data, false)
}

// EncodeBizMsg wraps an encoded business payload as a request ConnMsg
func EncodeBizMsg(service, method, reqID string, body []byte) ([]byte, error) {
	return connRequest(CmdType["Request"], method, reqID, service, body)
}

// DecodeBizMsg decodes a ConnMsg into service, method, req_id, body, is_response and head
func DecodeBizMsg(data []byte) (*pyjson.Object, error) {
	result, err := DecodeConnMsg(data)
	if err != nil {
		return nil, err
	}
	head := result.Vals["head"].(*pyjson.Object)
	out := pyjson.NewObject()
	out.Set("service", head.Vals["module"])
	out.Set("method", head.Vals["cmd"])
	out.Set("req_id", head.Vals["msg_id"])
	out.Set("body", result.Vals["data"])
	out.Set("is_response", pyjson.Equal(head.Vals["cmd_type"], CmdType["Response"]))
	out.Set("head", head)
	return out, nil
}

// Biz request whose req_id is the msg_id, else prefix_seq with seq taken before the conn seq_no
func bizRequest(method, prefix string, body []byte, msgID string) ([]byte, error) {
	if msgID == "" {
		msgID = fmt.Sprintf("%s_%d", prefix, NextSeqNo())
	}
	return EncodeBizMsg(bizPkg, method, msgID, body)
}

var (
	msgContentSpec = []specField{{1, "text", 's'}, {2, "uuid", 's'}, {4, "data", 's'}, {5, "desc", 's'}, {6, "ext", 's'},
		{7, "sound", 's'}, {10, "url", 's'}, {12, "file_name", 's'}, {3, "image_format", 'i'}, {9, "index", 'i'}, {11, "file_size", 'i'}}
	imageInfoSpec         = []specField{{1, "type", 'i'}, {2, "size", 'i'}, {3, "width", 'i'}, {4, "height", 'i'}, {5, "url", 'r'}}
	mapEntrySpec          = []specField{{1, "key", 's'}, {2, "value", 's'}}
	forwardMultimediaSpec = []specField{{1, "type", 's'}, {2, "url", 's'}, {4, "file_name", 's'}, {5, "file_size", 'i'}, {15, "media_id", 's'}}
)

func encodeMsgContent(contentValue any) ([]byte, error) {
	buf, err := encodeSpec(contentValue, msgContentSpec)
	if err != nil {
		return nil, err
	}
	content, _ := asDict(contentValue)
	p := &pbuf{b: buf}
	if imgs := content["image_info_array"]; pyrepr.Truthy(imgs) {
		list, ok := imgs.([]any)
		if !ok {
			return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(imgs))
		}
		for _, img := range list {
			enc, err := encodeSpec(img, imageInfoSpec)
			if err != nil {
				return nil, err
			}
			p.m(8, enc)
		}
	}
	if extMap, ok := asDict(content["ext_map"]); ok {
		for _, k := range dictKeys(content["ext_map"]) {
			entry := map[string]any{"key": k, "value": pyrepr.Str(extMap[k])}
			enc, err := encodeSpec(entry, mapEntrySpec)
			if err != nil {
				return nil, err
			}
			p.m(999, enc)
		}
	}
	return p.b, p.err
}

func decodeMsgContent(data []byte) (*pyjson.Object, error) {
	fd, err := parseDict(data)
	if err != nil {
		return nil, err
	}
	content := decodeSpec(fd, msgContentSpec)
	rep, err := fd.parseRepeated(8)
	if err != nil {
		return nil, err
	}
	imgs := []any{}
	for _, d := range rep {
		if img := decodeSpec(d, imageInfoSpec); img.Len() > 0 {
			imgs = append(imgs, img)
		}
	}
	entries, err := fd.parseRepeated(999)
	if err != nil {
		return nil, err
	}
	extMap := pyjson.NewObject()
	for _, e := range entries {
		if k := e.GetString(1); k != "" {
			extMap.Set(k, e.GetString(2))
		}
	}
	if len(imgs) > 0 {
		content.Set("image_info_array", imgs)
	}
	if extMap.Len() > 0 {
		content.Set("ext_map", extMap)
	}
	return content, nil
}

// EncodeMsgBodyElement encodes a msg_type and msg_content element
func EncodeMsgBodyElement(elementValue any) ([]byte, error) {
	element, ok := asDict(elementValue)
	if !ok {
		return nil, typeErr("get", elementValue)
	}
	content, present := element["msg_content"]
	if !present {
		content = map[string]any{}
	}
	var contentBytes []byte
	if pyrepr.Truthy(content) {
		enc, err := encodeMsgContent(content)
		if err != nil {
			return nil, err
		}
		contentBytes = enc
	}
	msgType, present := element["msg_type"]
	if !present {
		msgType = ""
	}
	return encodeParts([]part{{1, 's', msgType}, {2, 'm', contentBytes}})
}

// DecodeMsgBodyElement decodes a body element into msg_type and msg_content
func DecodeMsgBodyElement(data []byte) (*pyjson.Object, error) {
	fd, err := parseDict(data)
	if err != nil {
		return nil, err
	}
	out := pyjson.NewObject()
	out.Set("msg_type", fd.GetString(1))
	content := pyjson.NewObject()
	if b := fd.GetBytes(2); len(b) > 0 {
		if content, err = decodeMsgContent(b); err != nil {
			return nil, err
		}
	}
	out.Set("msg_content", content)
	return out, nil
}

type pushField struct {
	fn  int64
	key string
	get func(FieldDict, int64) (any, error)
}

func getStr(fd FieldDict, fn int64) (any, error) { return fd.GetString(fn), nil }
func getVar(fd FieldDict, fn int64) (any, error) { return fd.GetVarint(fn), nil }

var inboundPushSpec = []pushField{
	{1, "callback_command", getStr}, {2, "from_account", getStr}, {3, "to_account", getStr},
	{4, "sender_nickname", getStr}, {5, "group_id", getStr}, {6, "group_code", getStr},
	{7, "group_name", getStr}, {8, "msg_seq", getVar}, {9, "msg_random", getVar},
	{10, "msg_time", getVar}, {11, "msg_key", getStr}, {12, "msg_id", getStr},
	{13, "msg_body", func(fd FieldDict, fn int64) (any, error) {
		out := []any{}
		for _, b := range fd.GetRepeatedBytes(fn) {
			el, err := DecodeMsgBodyElement(b)
			if err != nil {
				return nil, err
			}
			out = append(out, el)
		}
		return out, nil
	}},
	{14, "cloud_custom_data", getStr}, {15, "event_time", getVar}, {16, "bot_owner_id", getStr},
	{17, "recall_msg_seq_list", func(fd FieldDict, fn int64) (any, error) {
		rep, err := fd.parseRepeated(fn)
		if err != nil || len(rep) == 0 {
			return nil, err
		}
		out := []any{}
		for _, d := range rep {
			item := pyjson.NewObject()
			item.Set("msg_seq", d.GetVarint(1))
			item.Set("msg_id", d.GetString(2))
			out = append(out, item)
		}
		return out, nil
	}},
	{18, "claw_msg_type", getVar}, {19, "private_from_group_code", getStr},
	{20, "trace_id", func(fd FieldDict, fn int64) (any, error) {
		b := fd.GetBytes(fn)
		if len(b) == 0 {
			return "", nil
		}
		inner, err := parseDict(b)
		if err != nil {
			return nil, err
		}
		return inner.GetString(1), nil
	}},
}

// DecodeInboundPush parses an InboundMessagePush payload dropping empty values except msg_body and msg_seq, nil on failure
func DecodeInboundPush(data []byte) *pyjson.Object {
	fd, err := parseDict(data)
	if err != nil {
		return nil
	}
	out := pyjson.NewObject()
	for _, f := range inboundPushSpec {
		v, err := f.get(fd, f.fn)
		if err != nil {
			return nil
		}
		if truthyNum(v) || f.key == "msg_body" || f.key == "msg_seq" {
			out.Set(f.key, v)
		}
	}
	return out
}

func decodeForwardMsgContent(data []byte) (*pyjson.Object, error) {
	fd, err := parseDict(data)
	if err != nil {
		return nil, err
	}
	content := pyjson.NewObject()
	content.Set("type", fd.GetVarint(1))
	if s := fd.GetString(2); s != "" {
		content.Set("text", s)
	}
	if len(fd.GetRepeatedBytes(3)) > 0 {
		rep, err := fd.parseRepeated(3)
		if err != nil {
			return nil, err
		}
		media := []any{}
		for _, d := range rep {
			media = append(media, decodeSpec(d, forwardMultimediaSpec))
		}
		content.Set("multimedia", media)
	}
	return content, nil
}

// DecodeForwardMsgData parses ForwardMsgData bytes into sub_type, times, nick_name and msg, nil on failure
func DecodeForwardMsgData(data []byte) *pyjson.Object {
	fd, err := parseDict(data)
	if err != nil {
		return nil
	}
	rep, err := fd.parseRepeated(5)
	if err != nil {
		return nil
	}
	msgs := []any{}
	for _, d := range rep {
		contents := []any{}
		for _, b := range d.GetRepeatedBytes(4) {
			c, err := decodeForwardMsgContent(b)
			if err != nil {
				return nil
			}
			contents = append(contents, c)
		}
		m := pyjson.NewObject()
		m.Set("sender", d.GetString(1))
		m.Set("time", d.GetVarint(2))
		m.Set("plainText", d.GetString(3))
		m.Set("msgContent", contents)
		msgs = append(msgs, m)
	}
	out := pyjson.NewObject()
	out.Set("sub_type", fd.GetVarint(1))
	out.Set("begin_time", fd.GetVarint(2))
	out.Set("end_time", fd.GetVarint(3))
	out.Set("nick_name", fd.GetString(4))
	out.Set("msg", msgs)
	return out
}

func optInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// EncodeSendC2CMessage builds a SendC2CMessageReq ConnMsg, msg_id doubling as req_id when set
func EncodeSendC2CMessage(toAccount string, msgBody []any, fromAccount, msgID string, msgRandom int64, msgSeq *int64, groupCode, traceID string) ([]byte, error) {
	body, err := encodeParts([]part{{1, 's', msgID}, {2, 'S', toAccount}, {3, 's', fromAccount}, {4, 'v', msgRandom},
		{5, 'b', msgBody}, {6, 's', groupCode}, {7, 'n', optInt(msgSeq)}, {8, 't', traceID}})
	if err != nil {
		return nil, err
	}
	return bizRequest("send_c2c_message", "c2c", body, msgID)
}

// EncodeSendGroupMessage builds a SendGroupMessageReq ConnMsg, ref_msg_id naming the quoted message
func EncodeSendGroupMessage(groupCode string, msgBody []any, fromAccount, msgID, toAccount, random string, msgSeq *int64, refMsgID, traceID string) ([]byte, error) {
	body, err := encodeParts([]part{{1, 's', msgID}, {2, 'S', groupCode}, {3, 's', fromAccount}, {4, 's', toAccount}, {5, 's', random},
		{6, 'b', msgBody}, {7, 's', refMsgID}, {8, 'n', optInt(msgSeq)}, {9, 't', traceID}})
	if err != nil {
		return nil, err
	}
	return bizRequest("send_group_message", "grp", body, msgID)
}

// EncodeAuthBind builds the auth-bind request with auth info, device info and the route env
func EncodeAuthBind(bizID, uid, source, token, msgID, appVersion, operationSystem, botVersion, routeEnv string) ([]byte, error) {
	dev, err := encodeParts([]part{{1, 's', appVersion}, {2, 's', operationSystem}, {10, 'S', fmt.Sprint(IcebathInstanceID)}, {24, 's', botVersion}})
	if err != nil {
		return nil, err
	}
	auth := &pbuf{}
	auth.s(1, uid)
	auth.s(2, source)
	auth.s(3, token)
	if auth.err != nil {
		return nil, auth.err
	}
	req, err := encodeParts([]part{{1, 'S', bizID}, {2, 'm', auth.b}, {3, 'm', dev}, {5, 's', routeEnv}})
	if err != nil {
		return nil, err
	}
	return connRequest(CmdType["Request"], Cmd["AuthBind"], msgID, Module["ConnAccess"], req)
}

// EncodePing builds the empty ping request
func EncodePing(msgID string) ([]byte, error) {
	return connRequest(CmdType["Request"], Cmd["Ping"], msgID, Module["ConnAccess"], nil)
}

// EncodePushAck echoes the cmd, msg_id and module of the original push head
func EncodePushAck(originalHead any) ([]byte, error) {
	head, ok := asDict(originalHead)
	if !ok {
		return nil, typeErr("get", originalHead)
	}
	fields := make([]string, 3)
	for i, k := range []string{"cmd", "msg_id", "module"} {
		v, present := head[k]
		if !present {
			v = ""
		}
		s, ok := v.(string)
		if !ok && pyrepr.Truthy(v) {
			return nil, typeErr("encode", v)
		}
		fields[i] = s
	}
	return connRequest(CmdType["PushAck"], fields[0], fields[1], fields[2], nil)
}

// EncodeSendPrivateHeartbeat builds a SendPrivateHeartbeatReq ConnMsg
func EncodeSendPrivateHeartbeat(fromAccount, toAccount string, heartbeat int64) ([]byte, error) {
	p := &pbuf{}
	p.s(1, fromAccount)
	p.s(2, toAccount)
	p.v(3, big.NewInt(heartbeat))
	if p.err != nil {
		return nil, p.err
	}
	return bizRequest("send_private_heartbeat", "hb_priv", p.b, "")
}

// Swappable clock for the group heartbeat send_time
var nowMillis = func() int64 { return time.Now().UnixMilli() }

// EncodeSendGroupHeartbeat builds a SendGroupHeartbeatReq ConnMsg, a zero send_time meaning now in ms
func EncodeSendGroupHeartbeat(fromAccount, groupCode string, heartbeat, sendTime int64) ([]byte, error) {
	ts := sendTime
	if ts == 0 {
		ts = nowMillis()
	}
	p := &pbuf{}
	p.s(1, fromAccount)
	p.s(2, "")
	p.s(3, groupCode)
	p.v(4, big.NewInt(ts))
	p.v(5, big.NewInt(heartbeat))
	if p.err != nil {
		return nil, p.err
	}
	return bizRequest("send_group_heartbeat", "hb_grp", p.b, "")
}

// EncodeQueryGroupInfo builds a QueryGroupInfoReq ConnMsg
func EncodeQueryGroupInfo(groupCode string) ([]byte, error) {
	p := &pbuf{}
	p.s(1, groupCode)
	if p.err != nil {
		return nil, p.err
	}
	return bizRequest("query_group_info", "qgi", p.b, "")
}

// DecodeQueryGroupInfoRsp parses code, message and the group info fields, nil on failure
func DecodeQueryGroupInfoRsp(data []byte) *pyjson.Object {
	fd, err := parseDict(data)
	if err != nil {
		return nil
	}
	result := pyjson.NewObject()
	result.Set("code", fd.GetVarint(1))
	if s := fd.GetString(2); s != "" {
		result.Set("message", s)
	}
	gi := FieldDict{}
	if entries := fd[fieldKey(int64(3))]; len(entries) > 0 {
		if b, ok := entries[0].value.([]byte); ok && len(b) > 0 {
			if gi, err = parseDict(b); err != nil {
				return nil
			}
		}
	}
	result.Set("group_name", gi.GetString(1))
	result.Set("owner_id", gi.GetString(2))
	result.Set("owner_nickname", gi.GetString(3))
	result.Set("member_count", gi.GetVarint(4))
	return result
}

// EncodeGetGroupMemberList builds a GetGroupMemberListReq ConnMsg, offset omitted when zero
func EncodeGetGroupMemberList(groupCode string, offset, limit int64) ([]byte, error) {
	p := &pbuf{}
	p.s(1, groupCode)
	if offset != 0 {
		p.v(2, big.NewInt(offset))
	}
	p.v(3, big.NewInt(limit))
	if p.err != nil {
		return nil, p.err
	}
	return bizRequest("get_group_member_list", "gml", p.b, "")
}

// DecodeGetGroupMemberListRsp parses code, message, members without empty fields except role, next_offset and is_complete
func DecodeGetGroupMemberListRsp(data []byte) *pyjson.Object {
	fd, err := parseDict(data)
	if err != nil {
		return nil
	}
	rep, err := fd.parseRepeated(3)
	if err != nil {
		return nil
	}
	members := []any{}
	for _, m := range rep {
		mem := pyjson.NewObject()
		for _, kv := range []struct {
			k string
			v any
		}{{"user_id", m.GetString(1)}, {"nickname", m.GetString(2)}, {"role", m.GetVarint(3)}, {"join_time", m.GetVarint(4)}, {"name_card", m.GetString(5)}} {
			if truthyNum(kv.v) || kv.k == "role" {
				mem.Set(kv.k, kv.v)
			}
		}
		members = append(members, mem)
	}
	out := pyjson.NewObject()
	out.Set("code", fd.GetVarint(1))
	out.Set("message", fd.GetString(2))
	out.Set("members", members)
	out.Set("next_offset", fd.GetVarint(4))
	out.Set("is_complete", truthyNum(fd.GetVarint(5)))
	return out
}
