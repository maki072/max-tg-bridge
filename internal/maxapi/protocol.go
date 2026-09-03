package maxapi

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// Protocol constants for the MAX WebSocket transport (ver=11, JSON frames).
// Source: reverse-engineered docs at github.com/pr0bel1230/max-api-docs.
const (
	ProtocolVersion = 11

	// cmd values
	CmdRequest = 0 // client->server request OR server->client push
	CmdAck     = 1 // successful response
	CmdError   = 3 // error response
)

// Opcodes. Full known set from the reverse-engineered docs
// (github.com/pr0bel1230/max-api-docs). Not all are used by this service, but
// they are documented here for reference. cmd=0 request/push, cmd=1 ack, cmd=3 error.
const (
	// Session / auth
	OpPing                = 1   // PING / heartbeat, both directions
	OpInit                = 6   // INIT — session init (deviceId + userAgent)
	OpVerificationRequest = 17  // VERIFICATION_REQUEST — request SMS code
	OpCodeEnter           = 18  // CODE_ENTER — submit SMS/QR code
	OpLogin               = 19  // LOGIN — authorize with token
	OpAuthConfirm         = 20  // AUTH_CONFIRM — registration confirm
	OpConfig              = 101 // CONFIG — server config
	OpServerTime          = 200 // SERVER_TIME
	OpCaptchaRequest      = 224 // CAPTCHA_REQUEST — request VK captcha link
	OpQRAuthRequest       = 288 // QR_AUTH_REQUEST
	OpQRAuthPoll          = 289 // QR_AUTH_POLL

	// Contacts / profile / presence
	OpGetContacts      = 32  // GET_CONTACTS / GET_CONTACT_DETAILS ({contactIds:[...]})
	OpContactsPresence = 35  // CONTACTS_PRESENCE
	OpGetBlocked       = 36  // GET_BLOCKED
	OpFindByPhone      = 46  // FIND_BY_PHONE_NUMBER
	OpGetUserInfo      = 177 // GET_USERINFO
	OpGetCommonChats   = 198 // GET_COMMON_CHATS
	OpGetSessions      = 96  // GET_SESSIONS

	// Chats
	OpGetChatsBulk  = 48  // GET_CHATS_BULK
	OpGetChats      = 53  // GET_CHATS
	OpVoid          = 55  // VOID / chats heartbeat
	OpGetChatInfo   = 61  // GET_CHAT_INFO ({chatId}) -> {chat:{title,type,...}}
	OpSubscribe     = 75  // SUBSCRIBE_TO_CHAT ({chatId, subscribe})
	OpChatOperation = 77  // CHAT_OPERATION
	OpChatShow      = 86  // CHAT_SHOW
	OpChatActivity  = 92  // CHAT_ACTIVITY
	OpGetFolders    = 272 // GET_FOLDERS

	// Messages
	OpGetHistory     = 49 // GET_HISTORY
	OpMarkAsRead     = 50 // MARK_AS_READ
	OpMsgSend        = 64 // MSG_SEND — send a message
	OpMsgTyping      = 65 // MSG_TYPING
	OpMsgDelete      = 66 // MSG_DELETE
	OpMsgEdit        = 67 // MSG_EDIT
	OpForwardMessage = 70 // FORWARD_MESSAGE
	OpSearchMessages = 73 // SEARCH_MESSAGES

	// Media / files / stickers
	OpGetPresetAvatars = 25  // GET_PRESET_AVATARS
	OpStickerSync      = 26  // STICKER_SYNC
	OpStickerData      = 27  // STICKER_DATA
	OpAnimojiGet       = 28  // ANIMOJI_AND_STICKER_GET
	OpImageUploadURL   = 80  // IMAGE_UPLOAD_URL
	OpImageUploadSmile = 81  // IMAGE_UPLOAD_IUSMILE
	OpFileUpload       = 87  // FILE_UPLOAD
	OpGetBanners       = 302 // GET_BANNERS

	// Calls
	OpCallEdit    = 69  // CALL_EDIT
	OpCallStart   = 78  // CALL_START
	OpCallHistory = 79  // CALL_HISTORY
	OpCallToken   = 158 // CALL_TOKEN

	// Server -> client pushes
	OpPushNewMessage     = 128 // NEW_MESSAGE (incoming message)
	OpPushTyping         = 129 // NOTIF_TYPING
	OpPushRead           = 130 // NOTIF_READ
	OpPushPresence       = 132 // NOTIF_PRESENCE
	OpPushAttach         = 136 // NOTIF_ATTACH
	OpPushIncomingCall   = 137 // NOTIF_INCOMING_CALL
	OpPushMsgDeleteRange = 140 // NOTIF_MSG_DELETE_RANGE
	OpPushMsgDelete      = 142 // NOTIF_MSG_DELETE
	OpPushReactionFlat   = 155 // reaction push, flat payload (confirmed live: {chatId,messageId,counters,totalCount})
	OpPushReaction       = 156 // NOTIF_REACTION, payload nested under "reactionInfo" per docs
)

// nameEntry / contactInfo mirror the MAX contact shape: names is an array of
// {name, type} where type is FULL_NAME / FIRST_NAME / LAST_NAME.
type nameEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type contactInfo struct {
	ID    json.Number `json:"id"`
	Names []nameEntry `json:"names"`
}

// pickName returns the best display name from a names array (prefer FULL_NAME).
func pickName(names []nameEntry) string {
	for _, n := range names {
		if n.Type == "FULL_NAME" && n.Name != "" {
			return n.Name
		}
	}
	for _, n := range names {
		if n.Name != "" {
			return n.Name
		}
	}
	return ""
}

// ignoredPushOpcodes are server->client pushes that carry a "message"-like
// object but are NOT new incoming messages, so the heuristic must skip them.
// NOTE: opcode 128 is NEW_MESSAGE (confirmed against live frames and the
// MaxBridge reference) — it must NOT be ignored. It is the incoming-message
// opcode (see config default IncomingOpcode=128).
var ignoredPushOpcodes = map[int]bool{
	1:   true, // PING
	129: true, // NOTIF_TYPING
	130: true, // NOTIF_READ
	131: true, // unknown
	132: true, // NOTIF_PRESENCE
	136: true, // NOTIF_ATTACH
	137: true, // NOTIF_INCOMING_CALL
	140: true, // NOTIF_MSG_DELETE_RANGE
	142: true, // NOTIF_MSG_DELETE
	// 156 (NOTIF_REACTION) is handled explicitly in handlePush, not ignored.
}

// ReactionCounter is one emoji's tally on a message.
type ReactionCounter struct {
	Reaction string `json:"reaction"`
	Count    int    `json:"count"`
}

// reactionPush is the wire shape of a reaction push. Two variants seen live:
// opcode 156 nests counters under "reactionInfo" (matches the docs); opcode
// 155 puts them flat at the top level. Both are parsed into the same struct
// so tryReaction can just prefer whichever side came populated.
type reactionPush struct {
	ChatID       json.Number       `json:"chatId"`
	MessageID    json.Number       `json:"messageId"`
	Counters     []ReactionCounter `json:"counters"`   // opcode 155 (flat)
	TotalCount   int               `json:"totalCount"` // opcode 155 (flat)
	ReactionInfo *struct {
		Counters     []ReactionCounter `json:"counters"`
		YourReaction string            `json:"yourReaction"`
		TotalCount   int               `json:"totalCount"`
	} `json:"reactionInfo"` // opcode 156 (nested)
}

// IncomingReaction is a normalized NOTIF_REACTION event forwarded to the bridge.
type IncomingReaction struct {
	ChatID     int64
	MessageID  string
	Counters   []ReactionCounter
	TotalCount int
}

// Frame is a single WebSocket JSON frame.
type Frame struct {
	Ver     int             `json:"ver"`
	Cmd     int             `json:"cmd"`
	Seq     int             `json:"seq"`
	Opcode  int             `json:"opcode"`
	Payload json.RawMessage `json:"payload"`
}

// UserAgent is the client descriptor sent in INIT.
type UserAgent struct {
	DeviceType      string `json:"deviceType"`
	PushDeviceType  string `json:"pushDeviceType"`
	Locale          string `json:"locale"`
	DeviceLocale    string `json:"deviceLocale"`
	OSVersion       string `json:"osVersion"`
	DeviceName      string `json:"deviceName"`
	HeaderUserAgent string `json:"headerUserAgent"`
	AppVersion      string `json:"appVersion"`
	Screen          string `json:"screen"`
	Timezone        string `json:"timezone"`
}

// defaultUserAgent returns a plausible WEB client descriptor.
func defaultUserAgent() UserAgent {
	return UserAgent{
		DeviceType:      "WEB",
		PushDeviceType:  "WEBPUSH",
		Locale:          "ru",
		DeviceLocale:    "ru",
		OSVersion:       "Linux",
		DeviceName:      "Firefox",
		HeaderUserAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
		AppVersion:      "25.11.1",
		Screen:          "1080x1920 1.0x",
		Timezone:        "Europe/Moscow",
	}
}

// Attachment is a normalized media attachment of an incoming message.
type Attachment struct {
	Kind     string // "photo" | "file" | "video" | "voice" | "poll" | "other"
	URL      string // direct, publicly fetchable URL (photo/file/video only)
	Name     string // file name (for documents) or a human label for other kinds
	Duration int64  // duration in ms (voice notes)
	Text     string // extra text (e.g. a poll question, when the server includes one)
}

// IncomingMessage is a normalized new message forwarded to the bridge.
type IncomingMessage struct {
	ChatID      int64
	ChatTitle   string
	SenderID    int64
	SenderName  string
	MessageID   string
	Text        string
	Attachments []Attachment
	Opcode      int  // raw push opcode it arrived on (useful for discovery)
	Outgoing    bool // true if sent by us (self) from the MAX app/web
	Forwarded   bool // true if this is a forwarded message
}

// attach is a message attachment as MAX sends it. PHOTO exposes a public webp
// image at baseUrl; FILE/VIDEO expose fileUrl/url. Voice notes (sent as _type
// AUDIO, or UNSUPPORTED when the server decides our WEB device type can't
// render them) carry duration+audioId+token instead of a fetchable URL. Polls
// carry a question under one of a few possible field names — best-effort
// parsed since the wire schema for MAX polls is not documented anywhere.
type attach struct {
	Type     string `json:"_type"`
	BaseURL  string `json:"baseUrl"`
	FileURL  string `json:"fileUrl"`
	URL      string `json:"url"`
	Name     string `json:"name"`
	Duration int64  `json:"duration"` // ms, present on voice/audio attaches
	Text     string `json:"text"`     // best-effort: poll question, if present here
	Question string `json:"question"` // best-effort: poll question, alt field name
}

// msgBody is a message body (self-referential: a forwarded message nests the
// original under link.message, leaving the outer text/attaches empty).
type msgBody struct {
	ID       json.Number `json:"id"`
	Text     string      `json:"text"`
	Sender   json.Number `json:"sender"`
	Type     string      `json:"type"`
	Time     int64       `json:"time"`
	Attaches []attach    `json:"attaches"`
	Link     *struct {
		Type    string   `json:"type"`
		Message *msgBody `json:"message"`
	} `json:"link"`
}

// pushMessage is the minimal shape of a message-bearing push payload.
type pushMessage struct {
	ChatID  json.Number `json:"chatId"`
	Message *msgBody    `json:"message"`
}

// toAttachments normalizes MAX attaches into forwardable Attachments.
//
// IMPORTANT: every attach must turn into *something* — silently dropping an
// attach (as the old PHOTO/FILE/VIDEO-only switch did whenever a URL field
// was missing or the type was unrecognized) is what made voice messages,
// polls, and the occasional photo without a "baseUrl" vanish entirely: with
// no attachment produced, a message with empty text and a dropped attach
// looks like a contentless service push and tryMessage discards it outright.
// Unknown/undocumented types fall through to "other" so the bridge still
// posts a placeholder into Telegram instead of losing the message.
func toAttachments(as []attach) []Attachment {
	var out []Attachment
	for _, a := range as {
		switch a.Type {
		case "PHOTO":
			u := a.BaseURL
			if u == "" {
				u = a.URL
			}
			if u == "" {
				u = a.FileURL
			}
			if u != "" {
				out = append(out, Attachment{Kind: "photo", URL: u})
			} else {
				out = append(out, Attachment{Kind: "other", Name: "PHOTO"})
			}
		case "FILE":
			u := a.FileURL
			if u == "" {
				u = a.URL
			}
			if u != "" {
				out = append(out, Attachment{Kind: "file", URL: u, Name: a.Name})
			} else {
				out = append(out, Attachment{Kind: "other", Name: "FILE " + a.Name})
			}
		case "VIDEO":
			u := a.URL
			if u == "" {
				u = a.FileURL
			}
			if u != "" {
				out = append(out, Attachment{Kind: "video", URL: u, Name: a.Name})
			} else {
				out = append(out, Attachment{Kind: "other", Name: "VIDEO"})
			}
		case "AUDIO", "UNSUPPORTED":
			// Voice notes: the server sometimes reports the real type (AUDIO),
			// sometimes substitutes UNSUPPORTED for our WEB device type — both
			// carry a "duration" (ms) but no fetchable URL in either case.
			if a.Duration > 0 {
				out = append(out, Attachment{Kind: "voice", Duration: a.Duration})
			} else {
				out = append(out, Attachment{Kind: "other", Name: a.Type})
			}
		case "POLL":
			q := a.Text
			if q == "" {
				q = a.Question
			}
			out = append(out, Attachment{Kind: "poll", Text: q})
		case "CONTROL":
			// Service-only markers (e.g. "bot started") — not user content.
		default:
			out = append(out, Attachment{Kind: "other", Name: a.Type})
		}
	}
	return out
}

// NewDeviceID returns a fresh random UUIDv4 string, for callers (store,
// console) that need to mint our OWN persistent deviceId — deliberately
// independent from any browser's deviceId (see client.go session()).
func NewDeviceID() (string, error) { return newUUID() }

// newUUID returns a random UUIDv4 string for deviceId.
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// numToInt64 safely parses a json.Number to int64 (0 if empty/invalid).
func numToInt64(n json.Number) int64 {
	if n == "" {
		return 0
	}
	v, err := n.Int64()
	if err != nil {
		return 0
	}
	return v
}
