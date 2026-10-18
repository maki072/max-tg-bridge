package config

import "testing"

func TestUseTCP(t *testing.T) {
	auto := &Config{}
	if !auto.UseTCP(false, "") {
		t.Error("no token yet: the browserless TCP path is the default")
	}
	if !auto.UseTCP(true, "desktop") {
		t.Error("a token minted by the bridge's own sign-in uses TCP")
	}
	if auto.UseTCP(true, "web") || auto.UseTCP(true, "") {
		t.Error("a browser/legacy token keeps the web WebSocket")
	}
	if !(&Config{MaxTransport: "tcp"}).UseTCP(true, "web") {
		t.Error("max_transport=tcp forces TCP")
	}
	if (&Config{MaxTransport: " WS "}).UseTCP(false, "desktop") {
		t.Error("max_transport=ws forces the WebSocket")
	}
}

func TestPhoneNotRequired(t *testing.T) {
	c := &Config{TelegramBotToken: "123:abc"}
	if !c.Complete() {
		t.Error("only the bot token is required now; the phone is asked for by the bot (/login)")
	}
	if (&Config{Phone: "+7999"}).Complete() {
		t.Error("without a bot token the config is incomplete")
	}
}
