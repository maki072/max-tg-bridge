package maxapi

import (
	"encoding/json"
	"testing"
	"time"
)

// Shapes follow what MAX sends (values made up); LOCATION/CONTROL/callbackId per Telemax's live captures.
func TestLocationContactReferenceKeyboard(t *testing.T) {
	raw := `[
	 {"_type":"LOCATION","latitude":55.7558,"longitude":37.6173,"zoom":14},
	 {"_type":"CONTACT","contactId":193584517,"name":"Рустем","firstName":"Рустем"},
	 {"_type":"INLINE_KEYBOARD","callbackId":"cb-1","keyboard":{"buttons":[
	   [{"type":"CALLBACK","text":"Да","payload":"yes"},{"type":"LINK","text":"Сайт","url":"https://example.org"}],
	   [{"type":"OPEN_APP","text":"Открыть","webApp":"some_bot","payload":"p1"}]]}}
	]`
	var as []attach
	if err := json.Unmarshal([]byte(raw), &as); err != nil {
		t.Fatal(err)
	}
	got := toAttachments(as)
	if len(got) != 3 {
		t.Fatalf("got %d attachments: %+v", len(got), got)
	}
	if l := got[0]; l.Kind != "location" || l.Lat != 55.7558 || l.Lon != 37.6173 {
		t.Errorf("location: %+v", l)
	}
	if c := got[1]; c.Kind != "contact" || c.ContactID != 193584517 || c.Phone != "" || c.Name != "Рустем" {
		t.Errorf("contact reference: %+v", c)
	}
	k := got[2]
	if k.Kind != "keyboard" || k.CallbackID != "cb-1" || len(k.Buttons) != 2 || len(k.Buttons[0]) != 2 {
		t.Fatalf("keyboard: %+v", k)
	}
	if b := k.Buttons[0][0]; b.Type != "CALLBACK" || b.Payload != "yes" || b.Text != "Да" {
		t.Errorf("callback button: %+v", b)
	}
	if b := k.Buttons[1][0]; b.Type != "OPEN_APP" || b.WebApp != "some_bot" || b.Payload != "p1" {
		t.Errorf("open-app button: %+v", b)
	}
}

func TestIsChatClosed(t *testing.T) {
	parse := func(raw string) []attach {
		var as []attach
		if err := json.Unmarshal([]byte(raw), &as); err != nil {
			t.Fatal(err)
		}
		return as
	}
	if !isChatClosed(parse(`[{"_type":"CONTROL","event":"system","message":"Чат закрыт"}]`)) {
		t.Error("the closed-chat marker must be detected")
	}
	if !isChatClosed(parse(`[{"_type":"CONTROL","event":"system","shortMessage":"чат закрыт."}]`)) {
		t.Error("shortMessage, case and a trailing dot must be tolerated")
	}
	if isChatClosed(parse(`[{"_type":"CONTROL","event":"leave","message":"Чат закрыт"}]`)) {
		t.Error("only event=system counts")
	}
	if isChatClosed(parse(`[{"_type":"FILE","name":"Чат закрыт"}]`)) {
		t.Error("an ordinary attach must not count")
	}
}

func pushFrame(payload string) Frame {
	return Frame{Opcode: OpPushNewMessage, Payload: json.RawMessage(payload)}
}

func TestPushChatClosedAndRemoved(t *testing.T) {
	c := newClient(Options{OnMessage: func(IncomingMessage) {}})

	if !c.tryMessage(pushFrame(`{"chatId":77,"message":{"id":5,"sender":1,"attaches":[{"_type":"CONTROL","event":"system","message":"Чат закрыт"}]}}`)) {
		t.Fatal("chat-closed push must be consumed")
	}
	select {
	case m := <-c.deliver:
		if !m.ChatClosed || m.ChatID != 77 {
			t.Errorf("got %+v", m)
		}
	default:
		t.Fatal("no ChatClosed message queued")
	}

	if !c.tryMessage(pushFrame(`{"chatId":77,"message":{"id":42,"status":"REMOVED","text":"x"}}`)) {
		t.Fatal("REMOVED push must be consumed")
	}
	select {
	case d := <-c.deletes:
		if d.ChatID != 77 || len(d.MessageIDs) != 1 || d.MessageIDs[0] != "42" {
			t.Errorf("got %+v", d)
		}
	default:
		t.Fatal("no delete queued for the REMOVED push")
	}
	select {
	case m := <-c.deliver:
		t.Fatalf("a REMOVED push must not be delivered as a message: %+v", m)
	default:
	}
}

func TestPauseResume(t *testing.T) {
	c := newClient(Options{})
	if c.Paused() {
		t.Fatal("a fresh client is not paused")
	}
	c.Pause(time.Hour)
	until, ok := c.PausedUntil()
	if !ok || time.Until(until) < 59*time.Minute {
		t.Fatalf("pause not in force: %v %v", until, ok)
	}
	done := make(chan struct{})
	go func() { c.waitWhilePaused(); close(done) }()
	select {
	case <-done:
		t.Fatal("waitWhilePaused returned while paused")
	case <-time.After(50 * time.Millisecond):
	}
	if !c.Resume() {
		t.Fatal("Resume should report it was paused")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitWhilePaused did not wake on Resume")
	}
	if c.Resume() {
		t.Fatal("second Resume should report not paused")
	}

	c.Pause(30 * time.Millisecond) // a pause that runs out by itself
	c.waitWhilePaused()
	if c.Paused() {
		t.Fatal("pause should have expired")
	}
}

func TestStatusCallbackOnlyOnFlip(t *testing.T) {
	var calls []bool
	c := newClient(Options{OnStatus: func(ok bool) { calls = append(calls, ok) }})
	c.setStatus(false, "x") // still disconnected: no call
	c.setStatus(true, "")
	c.setStatus(true, "")
	c.setStatus(false, "boom")
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("calls = %v, want [true false]", calls)
	}
}
