package maxapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func fakeToken(c byte) string { return strings.Repeat(string(c), sessionTokenLen) }

func TestFindLongToken(t *testing.T) {
	tok := fakeToken('A')
	for name, payload := range map[string]string{
		"top level":     `{"token":"` + tok + `"}`,
		"nested":        `{"a":{"b":["x","` + tok + `"]}}`,
		"alphabet only": `{"t":"` + strings.Repeat("a-b_c.", 110) + `abc"}`, // 663 chars
	} {
		got := findLongToken(json.RawMessage(payload))
		if got == "" || len(got) != sessionTokenLen {
			t.Errorf("%s: got %q", name, got)
		}
	}
	// A 663-char channel post inside the account snapshot is user content, not a token.
	if got := findLongToken(json.RawMessage(`{"chats":[{"last":"` + fakeToken('B') + `"}],"profile":{"x":"` + fakeToken('C') + `"}}`)); got != "" {
		t.Errorf("snapshot branches must be skipped, got %q", got[:10])
	}
	if got := findLongToken(json.RawMessage(`{"text":"` + strings.Repeat("слово ", 111)[:sessionTokenLen] + `"}`)); got != "" {
		t.Error("obvious text must not pass for a token")
	}
	if findLongToken(json.RawMessage(`not json`)) != "" || findLongToken(json.RawMessage(`{"k":"short"}`)) != "" {
		t.Error("garbage and short strings give no token")
	}
}

func TestLoginTokenFromEnter(t *testing.T) {
	if got := loginTokenFromEnter(json.RawMessage(`{"tokenAttrs":{"LOGIN":{"token":"documented-token"}}}`)); got != "documented-token" {
		t.Errorf("documented shape: %q", got)
	}
	if got := loginTokenFromEnter(json.RawMessage(`{"other":{"t":"` + fakeToken('Z') + `"}}`)); len(got) != sessionTokenLen {
		t.Errorf("fallback to a token-shaped string: %q", got)
	}
	if got := loginTokenFromEnter(json.RawMessage(`{"passwordChallenge":{"trackId":"t1","hint":"pet"}}`)); got != "" {
		t.Errorf("a password challenge carries no token, got %q", got)
	}
}

func TestDescribeError(t *testing.T) {
	err := fmt.Errorf(`server error (opcode 17): {"error":"phone.invalid","localizedMessage":"Неверный номер"}`)
	if got := DescribeError(err); got != "Неверный номер" {
		t.Errorf("got %q", got)
	}
	if got := DescribeError(fmt.Errorf(`server error (opcode 17): {"error":"x.y"}`)); got != "x.y" {
		t.Errorf("got %q", got)
	}
	if got := DescribeError(fmt.Errorf("timeout waiting for reply")); got != "timeout waiting for reply" {
		t.Errorf("got %q", got)
	}
	if DescribeError(nil) != "" {
		t.Error("nil error is empty")
	}
}

func TestTCPFrameRoundTrip(t *testing.T) {
	// A request goes out with protocol byte 11 and reads back through the stream reader.
	f := Frame{Cmd: CmdRequest, Seq: 7, Opcode: OpLogin, Payload: json.RawMessage(`{"token":"t","chatsCount":50}`)}
	raw, err := encodeBinFrameVer(f, tcpProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	if raw[0] != 11 {
		t.Errorf("protocol byte = %d, want 11", raw[0])
	}
	// Two frames back to back in one stream, the second split awkwardly.
	stream := append(append([]byte{}, raw...), raw...)
	r := bufio.NewReader(bytes.NewReader(stream))
	for i := 0; i < 2; i++ {
		data, err := readTCPRaw(r)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		got, err := decodeBinFrame(data)
		if err != nil || got.Seq != 7 || got.Opcode != OpLogin {
			t.Fatalf("frame %d: %+v %v", i, got, err)
		}
		var p map[string]any
		_ = json.Unmarshal(got.Payload, &p)
		if p["token"] != "t" {
			t.Errorf("payload = %s", got.Payload)
		}
	}
	if _, err := readTCPRaw(r); err == nil {
		t.Error("EOF expected after the last frame")
	}
	// A desynced stream is reported, not silently consumed.
	if _, err := readTCPRaw(bufio.NewReader(bytes.NewReader([]byte("HTTP/1.1 400 Bad Request\r\n")))); err == nil {
		t.Error("garbage must be rejected")
	}
}

func TestBarePingHasNoBody(t *testing.T) {
	raw, err := encodeBinFrameVer(Frame{Cmd: CmdRequest, Opcode: OpPing}, tcpProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != binHeaderLen {
		t.Errorf("a payload-less PING must be header only, got %d bytes", len(raw))
	}
	c := newClient(Options{TCP: true})
	if f := c.pingFrame(); len(f.Payload) != 0 {
		t.Errorf("tcp ping payload = %s", f.Payload)
	}
	if f := newClient(Options{}).pingFrame(); string(f.Payload) != `{"interactive":true}` {
		t.Errorf("web ping payload = %s", f.Payload)
	}
}

func TestDecodeConcatenatedValues(t *testing.T) {
	// "null" followed by a map: the answer is the map (some replies carry two top-level values).
	body := []byte{0xc0, 0x81, 0xa1, 'k', 0x01}
	data := append([]byte{11, 1, 0, 1, 0, 6, 0, 0, 0, byte(len(body))}, body...)
	f, err := decodeBinFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Payload) != `{"k":1}` {
		t.Errorf("payload = %s", f.Payload)
	}
}

func TestReplyMatchedByOpcodeWhenSeqNotEchoed(t *testing.T) {
	c := newClient(Options{})
	ch1, ch2 := make(chan *Frame, 1), make(chan *Frame, 1)
	c.pending[5], c.pendOp[5] = ch1, OpGetContacts
	c.pending[9], c.pendOp[9] = ch2, OpMsgSend
	// Reply to MSG_SEND with a seq we never used.
	if got := c.takePending(Frame{Cmd: CmdAck, Seq: 777, Opcode: OpMsgSend}); got != ch2 {
		t.Fatal("reply should match the MSG_SEND request by opcode")
	}
	if _, still := c.pending[9]; still {
		t.Error("matched request must be forgotten")
	}
	// Echoed seq wins.
	if got := c.takePending(Frame{Cmd: CmdAck, Seq: 5, Opcode: OpGetContacts}); got != ch1 {
		t.Fatal("echoed seq should match directly")
	}
	if got := c.takePending(Frame{Cmd: CmdAck, Seq: 1, Opcode: OpPing}); got != nil {
		t.Error("an unrelated reply matches nothing")
	}
}

func TestEmbeddedRussianCAsParse(t *testing.T) {
	cfg := maxTCPTLSConfig("api2.oneme.ru")
	if cfg.RootCAs == nil || cfg.ServerName != "api2.oneme.ru" {
		t.Fatalf("bad TLS config: %+v", cfg)
	}
	if !strings.Contains(string(russianRootCA), "BEGIN CERTIFICATE") || !strings.Contains(string(russianSubCA), "BEGIN CERTIFICATE") {
		t.Fatal("embedded certificates missing")
	}
}

func TestPauseClosesTCPConn(t *testing.T) {
	c := newClient(Options{TCP: true})
	srv, cli := newPipe()
	c.tcpConn = cli
	c.Pause(time.Hour)
	if !srv.waitClosed(time.Second) {
		t.Error("Pause must close the TCP connection")
	}
}
