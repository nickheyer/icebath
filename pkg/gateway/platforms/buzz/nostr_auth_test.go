package buzz

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSchnorrSignBIP340Vectors(t *testing.T) {
	for _, v := range []struct{ key, pub, aux, msg, sig string }{
		{"0000000000000000000000000000000000000000000000000000000000000003", "F9308A019258C31049344F85F89D5229B531C845836F99B08601F113BCE036F9",
			"0000000000000000000000000000000000000000000000000000000000000000", "0000000000000000000000000000000000000000000000000000000000000000",
			"E907831F80848D1069A5371B402410364BDF1C5F8307B0084C55F1CE2DCA821525F66A4A85EA8B71E482A74F382D2CE5EBEEE8FDB2172F477DF4900D310536C0"},
		{"B7E151628AED2A6ABF7158809CF4F3C762E7160F38B4DA56A784D9045190CFEF", "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"0000000000000000000000000000000000000000000000000000000000000001", "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"6896BD60EEAE296DB48A229FF71DFE071BDE413E6D43F917DC8DCF8C78DE33418906D11AC976ABCCB20B091292BFF4EA897EFCB639EA871CFA95F6DE339E4B0A"},
	} {
		pub, err := PublicKeyHex(v.key)
		if err != nil || pub != strings.ToLower(v.pub) {
			t.Fatalf("pub %s %v", pub, err)
		}
		aux, _ := hex.DecodeString(v.aux)
		msg, _ := hex.DecodeString(v.msg)
		sig, err := SchnorrSign(msg, v.key, aux)
		if err != nil || hex.EncodeToString(sig) != strings.ToLower(v.sig) {
			t.Fatalf("sig %x %v", sig, err)
		}
	}
	if _, err := SchnorrSign(make([]byte, 31), "03", nil); err == nil || err.Error() != "BIP-340 signs a 32-byte message" {
		t.Fatal(err)
	}
	if _, err := SchnorrSign(make([]byte, 32), strings.Repeat("0", 63)+"3", []byte{1}); err == nil || err.Error() != "auxiliary randomness must be 32 bytes" {
		t.Fatal(err)
	}
	if a, b := mustSign(t), mustSign(t); bytes.Equal(a, b) {
		t.Fatal("fresh auxiliary randomness repeated a signature")
	}
}

func mustSign(t *testing.T) []byte {
	sig, err := SchnorrSign(make([]byte, 32), strings.Repeat("0", 63)+"3", nil)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func TestDecodePrivateKeyForms(t *testing.T) {
	want := "67dea2ed018072d675f5415ecfaed7d2597555e202d85b3d65ea4e58d2d92ffa"
	for _, in := range []string{"nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5", "  NSEC1VL029MGPSPEDVA04G90VLTKH6FVH240ZQTV9K0T9AF8935KE9LAQSNLFE5 ", want, "67de a2ed018072d675f5415ecfaed7d2597555e202d85b3d65ea4e58d2d92ffa"} {
		k, err := decodePrivateKey(in)
		if err != nil || hex.EncodeToString(be32(k)) != want {
			t.Fatalf("%q -> %v %v", in, k, err)
		}
	}
	for in, msg := range map[string]string{
		"nsec1VL029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5": "nsec cannot mix upper- and lowercase",
		"nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe6": "invalid nsec checksum",
		"nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfeb": "invalid character in nsec",
		"nsec1qqqqq":            "invalid nsec encoding",
		"zz":                    "private key must be 64 hex characters or nsec",
		"00ff":                  "private key must be 32 bytes",
		strings.Repeat("0", 64): "private key is outside the secp256k1 range",
		"FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141": "private key is outside the secp256k1 range",
	} {
		if _, err := decodePrivateKey(in); err == nil || err.Error() != msg {
			t.Fatalf("%q -> %v, want %s", in, err, msg)
		}
	}
}

func TestParseAuthTagAndBuildAuthEvent(t *testing.T) {
	if tag, err := ParseAuthTag(`["auth","a","b","c"]`, "L"); err != nil || len(tag) != 4 || tag[3] != "c" {
		t.Fatal(tag, err)
	}
	for raw, msg := range map[string]string{"{": "L is not valid JSON", `["auth","a","b"]`: "L must be a four-string auth tag", `["x","a","b","c"]`: "L must be a four-string auth tag", `["auth","a","b",1]`: "L must be a four-string auth tag"} {
		if _, err := ParseAuthTag(raw, "L"); err == nil || err.Error() != msg {
			t.Fatalf("%s -> %v", raw, err)
		}
	}
	ts := int64(1700000000)
	key := strings.Repeat("0", 63) + "3"
	ev, err := BuildAuthEvent(AuthEventParams{PrivateKey: key, Challenge: "chalé", RelayURL: "wss://r", AuthTagJSON: ` ["auth","o","k","s"] `, CreatedAt: &ts, AuxiliaryRandomness: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	pub := "f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9"
	serialized := `[0,"` + pub + `",1700000000,22242,[["relay","wss://r"],["challenge","chal` + "é" + `"],["auth","o","k","s"]],""]`
	id := sha256.Sum256([]byte(serialized))
	if ev.ID != hex.EncodeToString(id[:]) || ev.Pubkey != pub || ev.Kind != 22242 || ev.CreatedAt != ts || len(ev.Tags) != 3 || len(ev.Sig) != 128 {
		t.Fatalf("%+v", ev)
	}
	sig, _ := SchnorrSign(id[:], key, make([]byte, 32))
	if ev.Sig != hex.EncodeToString(sig) {
		t.Fatal("signature differs from signing the event id")
	}
	if _, err := BuildAuthEvent(AuthEventParams{PrivateKey: key, AuthTagJSON: "[1]"}); err == nil || err.Error() != "BUZZ_AUTH_TAG must be a four-string auth tag" {
		t.Fatal(err)
	}
}
