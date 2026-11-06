package yuanbao

import (
	"bytes"
	"math/big"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func get(o *pyjson.Object, keys ...string) any {
	var v any = o
	for _, k := range keys {
		v = v.(*pyjson.Object).Vals[k]
	}
	return v
}

func TestVarintSmallValues(t *testing.T) {
	for _, v := range []int64{0, 1, 127, 128, 255, 300, 16383, 16384, 1 << 21, 1 << 28} {
		enc := EncodeVarint(big.NewInt(v))
		dec, pos, err := DecodeVarint(enc, 0)
		if err != nil || dec != v || pos != len(enc) {
			t.Fatalf("round trip %d: %v %d %v", v, dec, pos, err)
		}
	}
}

func TestVarintKnownValues(t *testing.T) {
	if got := EncodeVarint(big.NewInt(300)); !bytes.Equal(got, []byte{0xAC, 0x02}) {
		t.Fatalf("got %x", got)
	}
	if got := EncodeVarint(big.NewInt(-1)); len(got) != 10 || got[9] != 0x01 {
		t.Fatalf("negative got %x", got)
	}
}

func TestVarintPartialDecode(t *testing.T) {
	data := append(append([]byte{0}, EncodeVarint(big.NewInt(300))...), 0)
	if v, pos, err := DecodeVarint(data, 1); err != nil || v != int64(300) || pos != 3 {
		t.Fatalf("got %v %d %v", v, pos, err)
	}
	if _, _, err := DecodeVarint(bytes.Repeat([]byte{0xFF}, 11), 0); err == nil || err.Error() != "varint too long" {
		t.Fatalf("long varint err = %v", err)
	}
	if v, _, _ := DecodeVarint(append(bytes.Repeat([]byte{0xFF}, 9), 0x7F), 0); v.(*big.Int).BitLen() != 70 {
		t.Fatalf("70 bit varint = %v", v)
	}
}

func TestConnCodecBasicRoundTrip(t *testing.T) {
	dec, err := DecodeConnMsg(EncodeConnMsg(0, 42, []byte("hello world")))
	if err != nil || get(dec, "msg_type") != int64(0) || get(dec, "seq_no") != int64(42) || !bytes.Equal(get(dec, "data").([]byte), []byte("hello world")) {
		t.Fatalf("got %v %v", dec, err)
	}
}

func TestConnCodecFixedBytesSimple(t *testing.T) {
	if enc := EncodeConnMsg(0, 1, nil); !bytes.Equal(enc, []byte{0x0a, 0x02, 0x18, 0x01}) {
		t.Fatalf("got %x", enc)
	}
}

func TestBizCodecRoundTrip(t *testing.T) {
	body := []byte("\x0a\x05hello")
	enc, err := EncodeBizMsg("trpc.yuanbao.example", "/im/send_c2c_msg", "req-001", body)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodeBizMsg(enc)
	if err != nil || get(dec, "service") != "trpc.yuanbao.example" || get(dec, "method") != "/im/send_c2c_msg" ||
		get(dec, "req_id") != "req-001" || !bytes.Equal(get(dec, "body").([]byte), body) || get(dec, "is_response") != false {
		t.Fatalf("got %v %v", dec, err)
	}
}

func TestBizCodecIsResponseFlag(t *testing.T) {
	enc, _ := EncodeConnMsgFull(CmdType["Response"], "/im/send_c2c_msg", 1, "rsp-001", "svc", []byte{1}, false)
	if dec, _ := DecodeBizMsg(enc); get(dec, "is_response") != true {
		t.Fatalf("got %v", dec)
	}
}

func TestMsgBodyElementTextRoundTrip(t *testing.T) {
	enc, err := EncodeMsgBodyElement(map[string]any{"msg_type": "TIMTextElem", "msg_content": map[string]any{"text": "Hello, 世界!"}})
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := DecodeMsgBodyElement(enc)
	if get(dec, "msg_type") != "TIMTextElem" || get(dec, "msg_content", "text") != "Hello, 世界!" {
		t.Fatalf("got %v", dec)
	}
}

func TestMsgBodyElementFileRoundTrip(t *testing.T) {
	el, _ := pyjson.Loads(`{"msg_type": "TIMFileElem", "msg_content": {"url": "https://example.com/file.pdf", "file_size": 204800, "file_name": "document.pdf"}}`)
	enc, err := EncodeMsgBodyElement(el)
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := DecodeMsgBodyElement(enc)
	if get(dec, "msg_content", "file_name") != "document.pdf" || get(dec, "msg_content", "file_size") != int64(204800) {
		t.Fatalf("got %v", dec)
	}
}

func TestMsgBodyElementFixedTextBytes(t *testing.T) {
	enc, _ := EncodeMsgBodyElement(map[string]any{"msg_type": "TIMTextElem", "msg_content": map[string]any{"text": "hi"}})
	inner := append([]byte{0x0a, 0x02}, "hi"...)
	want := append(append(append([]byte{0x0a, 11}, "TIMTextElem"...), 0x12, byte(len(inner))), inner...)
	if !bytes.Equal(enc, want) {
		t.Fatalf("got %x want %x", enc, want)
	}
}

func buildInboundPush(t *testing.T, from, to, groupCode, msgKey string, msgSeq int64, text string) []byte {
	el, err := EncodeMsgBodyElement(map[string]any{"msg_type": "TIMTextElem", "msg_content": map[string]any{"text": text}})
	if err != nil {
		t.Fatal(err)
	}
	str := func(s string) []byte { b, _ := EncodeString(s); return b }
	var buf []byte
	buf = append(buf, EncodeField(2, WTLen, str(from))...)
	buf = append(buf, EncodeField(3, WTLen, str(to))...)
	if groupCode != "" {
		buf = append(buf, EncodeField(6, WTLen, str(groupCode))...)
	}
	buf = append(buf, EncodeField(8, WTVarint, EncodeVarint(big.NewInt(msgSeq)))...)
	buf = append(buf, EncodeField(11, WTLen, str(msgKey))...)
	return append(buf, EncodeField(13, WTLen, EncodeMessage(el))...)
}

func TestDecodeInboundPushBasicC2CTextMessage(t *testing.T) {
	r := DecodeInboundPush(buildInboundPush(t, "alice", "bot", "", "k001", 100, "你好"))
	if r == nil || get(r, "from_account") != "alice" || get(r, "to_account") != "bot" || get(r, "msg_seq") != int64(100) || get(r, "msg_key") != "k001" {
		t.Fatalf("got %v", r)
	}
	body := get(r, "msg_body").([]any)
	if len(body) != 1 || get(body[0].(*pyjson.Object), "msg_type") != "TIMTextElem" || get(body[0].(*pyjson.Object), "msg_content", "text") != "你好" {
		t.Fatalf("body %v", body)
	}
}

func TestDecodeInboundPushReturnsNoneOnEmpty(t *testing.T) {
	r := DecodeInboundPush(nil)
	if r == nil || len(r.Keys) != 2 || get(r, "msg_seq") != int64(0) {
		t.Fatalf("got %v", r)
	}
	if DecodeInboundPush([]byte{0x0b}) != nil {
		t.Fatal("unknown wire type decoded")
	}
}

func TestDecodeInboundPushMultipleMsgBodyElements(t *testing.T) {
	el1, _ := EncodeMsgBodyElement(map[string]any{"msg_type": "TIMTextElem", "msg_content": map[string]any{"text": "part1"}})
	el2, _ := EncodeMsgBodyElement(map[string]any{"msg_type": "TIMTextElem", "msg_content": map[string]any{"text": "part2"}})
	buf := append(EncodeField(2, WTLen, []byte("\x05alice")), EncodeField(13, WTLen, EncodeMessage(el1))...)
	buf = append(buf, EncodeField(13, WTLen, EncodeMessage(el2))...)
	r := DecodeInboundPush(buf)
	body := get(r, "msg_body").([]any)
	if len(body) != 2 || get(body[0].(*pyjson.Object), "msg_content", "text") != "part1" || get(body[1].(*pyjson.Object), "msg_content", "text") != "part2" {
		t.Fatalf("got %v", r)
	}
}

func TestEncodeAuthBind(t *testing.T) {
	res, err := EncodeAuthBind("ybBot", "user_001", "app", "tok_abc", "auth-001", "1.0.0", "Linux", "0.1.0", "")
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := DecodeConnMsg(res)
	if get(dec, "head", "cmd") != "auth-bind" || get(dec, "head", "module") != "conn_access" || get(dec, "head", "msg_id") != "auth-001" || len(get(dec, "data").([]byte)) == 0 {
		t.Fatalf("got %v", dec)
	}
}

func TestEncodePing(t *testing.T) {
	res, _ := EncodePing("ping-001")
	if dec, _ := DecodeConnMsg(res); get(dec, "head", "cmd") != "ping" || get(dec, "head", "module") != "conn_access" {
		t.Fatalf("got %v", dec)
	}
}

func TestEncodePushAck(t *testing.T) {
	head := map[string]any{"cmd_type": CmdType["Push"], "cmd": "some-push", "seq_no": int64(100), "msg_id": "push-001", "module": "im_module", "need_ack": true, "status": int64(0)}
	res, _ := EncodePushAck(head)
	dec, _ := DecodeConnMsg(res)
	if get(dec, "head", "cmd_type") != CmdType["PushAck"] || get(dec, "head", "cmd") != "some-push" || get(dec, "head", "msg_id") != "push-001" {
		t.Fatalf("got %v", dec)
	}
}

func TestCmdTypeValues(t *testing.T) {
	if CmdType["Request"] != 0 || CmdType["Response"] != 1 || CmdType["Push"] != 2 || CmdType["PushAck"] != 3 {
		t.Fatalf("got %v", CmdType)
	}
}

func TestSeqNoMonotonic(t *testing.T) {
	a, b, c := NextSeqNo(), NextSeqNo(), NextSeqNo()
	if b <= a || c <= b {
		t.Fatalf("%d %d %d", a, b, c)
	}
}

func TestSeqNoThreadSafety(t *testing.T) {
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				v := NextSeqNo()
				mu.Lock()
				if seen[v] {
					t.Errorf("duplicate seq_no %d", v)
				}
				seen[v] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

func TestEndToEndSendRecvC2C(t *testing.T) {
	wire, err := EncodeSendC2CMessage("recv_user", []any{map[string]any{"msg_type": "TIMTextElem", "msg_content": map[string]any{"text": "端到端测试"}}}, "send_bot", "e2e-001", 0, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := DecodeConnMsg(wire)
	if get(dec, "head", "cmd") != "send_c2c_message" || get(dec, "head", "msg_id") != "e2e-001" {
		t.Fatalf("head %v", dec)
	}
	fields, _ := ParseFields(get(dec, "data").([]byte))
	fd := FieldsToDict(fields)
	if fd.GetString(2) != "recv_user" || fd.GetString(3) != "send_bot" {
		t.Fatalf("fields %v", fd)
	}
	els := fd.GetRepeatedBytes(5)
	if len(els) != 1 {
		t.Fatalf("elements %d", len(els))
	}
	el, _ := DecodeMsgBodyElement(els[0])
	if get(el, "msg_type") != "TIMTextElem" || get(el, "msg_content", "text") != "端到端测试" {
		t.Fatalf("element %v", el)
	}
}

func TestGroupInfoAndMemberListDecoders(t *testing.T) {
	gi := append(EncodeField(1, WTLen, EncodeMessage([]byte("g"))), EncodeField(4, WTVarint, EncodeVarint(big.NewInt(9)))...)
	rsp := append(EncodeField(2, WTLen, EncodeMessage([]byte("ok"))), EncodeField(3, WTLen, EncodeMessage(gi))...)
	got, _ := pyjson.Dumps(DecodeQueryGroupInfoRsp(rsp))
	if got != `{"code": 0, "message": "ok", "group_name": "g", "owner_id": "", "owner_nickname": "", "member_count": 9}` {
		t.Fatalf("group info %s", got)
	}
	mem := EncodeField(1, WTLen, EncodeMessage([]byte("u1")))
	list := append(EncodeField(3, WTLen, EncodeMessage(mem)), EncodeField(5, WTVarint, []byte{1})...)
	got, _ = pyjson.Dumps(DecodeGetGroupMemberListRsp(list))
	if got != `{"code": 0, "message": "", "members": [{"user_id": "u1", "role": 0}], "next_offset": 0, "is_complete": true}` {
		t.Fatalf("member list %s", got)
	}
}
