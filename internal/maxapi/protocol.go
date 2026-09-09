package maxapi

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"strings"
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

	// Reactions
	OpMsgReaction       = 178 // MSG_REACTION — add/replace our reaction on a message
	OpMsgCancelReaction = 179 // MSG_CANCEL_REACTION — remove our reaction
	OpMsgGetReactions   = 180 // MSG_GET_REACTIONS — bulk reaction lookup (unused for now)

	// Polls
	OpSendVote = 304 // SEND_VOTE — vote in a poll (unused for now, reserved)

	// Media / files / stickers
	OpGetPresetAvatars = 25  // GET_PRESET_AVATARS
	OpStickerSync      = 26  // STICKER_SYNC
	OpStickerData      = 27  // STICKER_DATA
	OpAnimojiGet       = 28  // ANIMOJI_AND_STICKER_GET
	OpImageUploadURL   = 80  // IMAGE_UPLOAD_URL / PHOTO_UPLOAD — request a photo upload slot
	OpImageUploadSmile = 81  // IMAGE_UPLOAD_IUSMILE / STICKER_UPLOAD
	OpVideoUpload      = 82  // VIDEO_UPLOAD — request an upload slot for video/voice/video-note
	OpVideoPlay        = 83  // VIDEO_PLAY — resolve a temporary playable URL for a VIDEO attach ({chatId,messageId,videoId} -> {MP4_*, EXTERNAL})
	OpFileUpload       = 87  // FILE_UPLOAD
	OpFileDownload     = 88  // FILE_DOWNLOAD — resolve a temporary URL for a FILE attach ({chatId,messageId,fileId} -> {url,unsafe})
	OpGetBanners       = 302 // GET_BANNERS
	// OpAudioPlay resolves a temporary URL for a voice/AUDIO attach, mirroring
	// OpVideoPlay/OpFileDownload ({chatId,messageId,audioId} -> presumably
	// {url,...}). Confirmed to exist in the reverse-engineered opcode table but
	// NOT documented/implemented by any known client (PyMax declares it but
	// never calls it) — used here as a best-effort attempt; see
	// Client.getAudioURL for the defensive parsing.
	OpAudioPlay = 301 // AUDIO_PLAY

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
	// 142 (NOTIF_MSG_DELETE) and 156 (NOTIF_REACTION) are handled explicitly
	// in handlePush, not ignored.
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

// deletePush is the wire shape of NOTIF_MSG_DELETE (opcode 142): a chat and
// one or more deleted message ids. The exact field name for the id list is
// unconfirmed (the reverse-engineered docs only flag a "cascading deletion
// risk"), so both a single "messageId" and a "messageIds" array are accepted
// defensively — whichever the server actually sends will parse.
type deletePush struct {
	ChatID     json.Number   `json:"chatId"`
	MessageID  json.Number   `json:"messageId"`
	MessageIDs []json.Number `json:"messageIds"`
}

// IncomingDelete is a normalized NOTIF_MSG_DELETE event forwarded to the bridge.
type IncomingDelete struct {
	ChatID     int64
	MessageIDs []string
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
	URL      string // direct, publicly fetchable URL (photo/file/video, and voice when MAX exposes one)
	Name     string // file name (for documents) or a human label for other kinds
	Duration int64  // duration in ms (voice notes)
	Text     string // extra text (e.g. a poll question, when the server includes one)

	// ResolveKind/ResolveID/ResolveToken are set when MAX gave only an
	// internal id (+ token) with no directly fetchable URL — the common case
	// for VIDEO/FILE and (previously unfixable) voice/AUDIO attaches.
	// Client.resolveAttachments (called off the read loop, from deliverLoop /
	// History) turns these into a real URL via VIDEO_PLAY / FILE_DOWNLOAD /
	// AUDIO_PLAY before the message reaches the bridge, filling in URL above.
	// Empty ResolveKind means there is nothing to resolve.
	ResolveKind  string // "video" | "file" | "audio"
	ResolveID    int64
	ResolveToken string
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
	Opcode      int    // raw push opcode it arrived on (useful for discovery)
	Outgoing    bool   // true if sent by us (self) from the MAX app/web
	Forwarded   bool   // true if this is a forwarded message
	ReplyToID   string // MAX message id this is a reply to ("" if not a reply)
}

// attach is a message attachment as MAX sends it. PHOTO exposes a public webp
// image at baseUrl; FILE/VIDEO expose fileUrl/url. Voice notes (sent as _type
// AUDIO, or UNSUPPORTED when the server decides our WEB device type can't
// render them) carry duration+audioId+token instead of a fetchable URL. Polls
// carry a question under one of a few possible field names — best-effort
// parsed since the wire schema for MAX polls is not documented anywhere.
type attach struct {
	Type     string      `json:"_type"`
	BaseURL  string      `json:"baseUrl"`
	FileURL  string      `json:"fileUrl"`
	URL      string      `json:"url"`
	Name     string      `json:"name"`
	Duration int64       `json:"duration"` // ms, present on voice/audio attaches
	Text     string      `json:"text"`     // best-effort: poll question, if present here
	Question string      `json:"question"` // best-effort: poll question, alt field name
	FileID   json.Number `json:"fileId"`   // FILE attaches: internal id, pairs with Token
	VideoID  json.Number `json:"videoId"`  // VIDEO attaches: internal id, pairs with Token
	AudioID  json.Number `json:"audioId"`  // AUDIO/UNSUPPORTED (voice) attaches: internal id, pairs with Token
	Token    string      `json:"token"`    // internal token that pairs with fileId/videoId/audioId
}

// msgLink is a message's "link" object: a FORWARD nests the original message
// under Message (leaving the outer text/attaches empty); a REPLY normally
// carries just the target MessageID (matches the MessageID field pymax's own
// ReplyLink/ForwardLink send — {type:"REPLY"|"FORWARD", messageId}), though
// some server versions may also nest the quoted Message.
type msgLink struct {
	Type      string      `json:"type"`
	MessageID json.Number `json:"messageId"`
	Message   *msgBody    `json:"message"`
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
	Link     *msgLink    `json:"link"`
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
			att := Attachment{Kind: "file", URL: u, Name: a.Name}
			if u == "" && a.FileID != "" {
				// No direct URL — resolve one lazily via FILE_DOWNLOAD (opcode 88).
				att.ResolveKind = "file"
				att.ResolveID = numToInt64(a.FileID)
				att.ResolveToken = a.Token
			}
			if att.URL == "" && att.ResolveKind == "" {
				out = append(out, Attachment{Kind: "other", Name: "FILE " + a.Name})
			} else {
				out = append(out, att)
			}
		case "VIDEO":
			u := a.URL
			if u == "" {
				u = a.FileURL
			}
			att := Attachment{Kind: "video", URL: u, Name: a.Name}
			if u == "" && a.VideoID != "" {
				// No direct URL — MAX web pushes normally omit one for video and
				// expect the client to call VIDEO_PLAY (opcode 83) instead.
				att.ResolveKind = "video"
				att.ResolveID = numToInt64(a.VideoID)
				att.ResolveToken = a.Token
			}
			if att.URL == "" && att.ResolveKind == "" {
				out = append(out, Attachment{Kind: "other", Name: "VIDEO"})
			} else {
				out = append(out, att)
			}
		case "AUDIO", "UNSUPPORTED":
			// Voice notes: the server sometimes reports the real type (AUDIO),
			// sometimes substitutes UNSUPPORTED for our WEB device type. Both
			// carry a "duration" (ms). There is usually no fetchable URL (only
			// an internal audioId/token) — resolve one lazily via the
			// best-effort AUDIO_PLAY (opcode 301); if that also comes up empty
			// the bridge falls back to the "open MAX" placeholder as before.
			u := a.URL
			if u == "" {
				u = a.FileURL
			}
			if u == "" {
				u = a.BaseURL
			}
			att := Attachment{Kind: "voice", URL: u, Duration: a.Duration, Name: a.Name}
			if u == "" && a.AudioID != "" {
				att.ResolveKind = "audio"
				att.ResolveID = numToInt64(a.AudioID)
				att.ResolveToken = a.Token
				log.Printf("[max] voice note without URL: type=%s duration=%dms audioId=%s hasToken=%t (will try AUDIO_PLAY)",
					a.Type, a.Duration, a.AudioID, a.Token != "")
			}
			if u == "" && att.ResolveKind == "" && a.Duration == 0 {
				out = append(out, Attachment{Kind: "other", Name: a.Type})
			} else {
				out = append(out, att)
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

// parseLink interprets a message's "link" object. For a FORWARD (or an
// unrecognized link type carrying nested content — kept for compatibility
// with the original forward-detection heuristic) it returns the nested
// message's text/attachments in place of the (empty) outer ones. For a REPLY
// it leaves text/attachments untouched and returns the target MAX message id.
func parseLink(link *msgLink, text string, attachments []Attachment) (outText string, outAttachments []Attachment, forwarded bool, replyToID string) {
	outText, outAttachments = text, attachments
	if link == nil {
		return
	}
	if link.Type == "REPLY" {
		if link.MessageID != "" {
			replyToID = link.MessageID.String()
		} else if link.Message != nil {
			replyToID = link.Message.ID.String()
		}
		return
	}
	if link.Message != nil && strings.TrimSpace(text) == "" && len(attachments) == 0 {
		outText = link.Message.Text
		outAttachments = toAttachments(link.Message.Attaches)
		forwarded = true
	}
	return
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
