package maxapi

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBinFrameRoundTrip(t *testing.T) {
	in := Frame{Cmd: CmdRequest, Seq: 513, Opcode: OpMsgSend, Payload: json.RawMessage(
		`{"chatId":-78009687158878,"message":{"text":"привет","cid":-1791555092385,"attaches":[]},"notify":true,"f":1.5,"n":null}`)}
	raw, err := encodeBinFrame(in)
	if err != nil {
		t.Fatal(err)
	}
	if raw[0] != binProtocolVersion || raw[6] != 0 {
		t.Fatalf("bad header % x", raw[:binHeaderLen])
	}
	out, err := decodeBinFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Seq != in.Seq || out.Opcode != in.Opcode || out.Cmd != in.Cmd {
		t.Fatalf("header mismatch: %+v", out)
	}
	var a, b any
	_ = json.Unmarshal(in.Payload, &a)
	_ = json.Unmarshal(out.Payload, &b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Fatalf("payload mismatch:\n%s\n%s", ja, jb)
	}
}

func TestDecodeCompressedFrameWithExtIDs(t *testing.T) {
	// msgpack {"id": ext1(uint64 459943346), "s": "abcdabcdabcdx"}, the string
	// LZ4-compressed via a match that overlaps its own output.
	payload := []byte{0x82, 0xa2, 'i', 'd', 0xc7, 0x09, 0x01, 0xcf, 0, 0, 0, 0, 0x1b, 0x6a, 0x2d, 0xb2,
		0xa1, 's', 0xad}
	block := append([]byte{byte(len(payload)+4) << 4}, payload...) // literals only up to "abcd"
	block[0] = 0xf0                                                // 15+ literals
	block = append(block[:1], append([]byte{byte(len(payload) + 4 - 15)}, block[1:]...)...)
	block = append(block, 'a', 'b', 'c', 'd', 0x04, 0x00) // match: offset 4
	block[0] |= 0x04                                      // match length 4+4 = 8
	block = append(block, 0x10, 'x')                      // final literal
	frame := []byte{binProtocolVersion, CmdAck, 0, 7, 0, 49, 2, 0, 0, byte(len(block))}
	frame = append(frame, block...)

	f, err := decodeBinFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":459943346,"s":"abcdabcdabcdx"}`; string(f.Payload) != want {
		t.Fatalf("payload = %s, want %s", f.Payload, want)
	}
}
