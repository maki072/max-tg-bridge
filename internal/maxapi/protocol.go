package maxapi

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
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
	OpGetChatsBulk  = 48 // GET_CHATS_BULK
	OpGetChats      = 53 // GET_CHATS
	OpVoid          = 55 // VOID / chats heartbeat
	OpGetChatInfo   = 61 // GET_CHAT_INFO ({chatId}) -> {chat:{title,type,...}}
	OpSubscribe     = 75 // SUBSCRIBE_TO_CHAT ({chatId, subscribe})
	OpChatOperation = 77 // CHAT_OPERATION

	// Chat management. Numbers and payload shapes come from the open-source
	// Telemax bridge (github.com/Trollobot/Telemax, AGPL-3.0), which confirmed
	// them live — only these wire facts are reused, the code here is our own.
	OpChatDelete   = 52  // CHAT_DELETE {chatId, lastEventTime, forAll}
	OpChatSetInfo  = 55  // {chatId, title?, description?} (older docs list 55 as a no-op VOID)
	OpChatLeave    = 58  // CHAT_LEAVE {chatId}
	OpChatMembers  = 59  // CHAT_MEMBERS {chatId, userIds, showHistory, operation:"add"|"remove"}
	OpPublicSearch = 60  // PUBLIC_SEARCH {query, count} — finds public bots and channels, not people
	OpChatShow     = 86  // CHAT_SHOW
	OpChatActivity = 92  // CHAT_ACTIVITY
	OpGetFolders   = 272 // GET_FOLDERS

	// Messages
	OpGetHistory     = 49 // GET_HISTORY
	OpMarkAsRead     = 50 // MARK_AS_READ
	OpMsgSend        = 64 // MSG_SEND — send a message
	OpMsgTyping      = 65 // MSG_TYPING
	OpMsgDelete      = 66 // MSG_DELETE
	OpMsgEdit        = 67 // MSG_EDIT
	OpForwardMessage = 70 // FORWARD_MESSAGE
	// OpMsgCallback presses a CALLBACK button of a bot's inline keyboard:
	// {callbackId, chatId, payload?} (shape per Telemax).
	OpMsgCallback    = 118
	OpSearchMessages = 73 // SEARCH_MESSAGES

	// Reactions
	OpMsgReaction       = 178 // MSG_REACTION — add/replace our reaction on a message
	OpMsgCancelReaction = 179 // MSG_CANCEL_REACTION — remove our reaction
	OpMsgGetReactions   = 180 // MSG_GET_REACTIONS — bulk reaction lookup (unused for now)

	// Polls
	OpSendVote = 304 // SEND_VOTE — vote in a poll (payload shape inferred, see Client.Vote)
	// OpPollUpdates is sent by web.max.ru every ~5s while a poll is on screen:
	// {chatId, polls:[{messageId, pollId}]} (confirmed from a live binary frame).
	OpPollUpdates = 306

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
	ID          json.Number     `json:"id"`
	Names       []nameEntry     `json:"names"`
	Phone       json.RawMessage `json:"phone"` // large integer on the wire (sometimes quoted)
	Link        string          `json:"link"`  // https://max.ru/<nick> — bots have one, ordinary users don't
	Description string          `json:"description"`
}

// rawText renders a raw JSON scalar (number or string) as plain text.
func rawText(r json.RawMessage) string {
	s := strings.Trim(strings.TrimSpace(string(r)), `"`)
	if s == "null" {
		return ""
	}
	return s
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
	IsPwa           bool   `json:"isPwa"` // sent (false) by the real web client
}

// AppVersionOverride, when non-empty (config "max_app_version"), replaces the
// default appVersion sent in INIT, e.g. to match the current web.max.ru
// version. (It does NOT make polls appear over the JSON transport — live
// testing showed that takes the binary transport, see binproto.go.)
var AppVersionOverride string

// defaultUserAgent returns a plausible WEB client descriptor.
func defaultUserAgent() UserAgent {
	ua := baseUserAgent()
	if AppVersionOverride != "" {
		ua.AppVersion = AppVersionOverride
	}
	// The settings screen shows "26.10.3 (19515)", but the web client sends
	// only "26.10.3" in INIT (confirmed from a live frame) — drop the build.
	if i := strings.Index(ua.AppVersion, " ("); i > 0 {
		ua.AppVersion = ua.AppVersion[:i]
	}
	return ua
}

func baseUserAgent() UserAgent {
	return UserAgent{
		DeviceType:      "WEB",
		PushDeviceType:  "WEBPUSH",
		Locale:          "ru",
		DeviceLocale:    "ru",
		OSVersion:       "Linux",
		DeviceName:      "Firefox",
		HeaderUserAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
		AppVersion:      "26.10.3", // web.max.ru as of 2026-10; MAX hides newer attach types (polls, voice) from older versions
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
	// Kind "video": Width/Height as MAX shows it (rotation applied; 0 when
	// unknown), Round for a "кружочек" video message (videoType 1), AltURLs the
	// remaining MP4 qualities (best first) to fall back to when URL won't download.
	Width, Height int
	Round         bool
	AltURLs       []string
	Text          string // extra text (e.g. a poll question, when the server includes one)
	Poll          *Poll  // set for Kind "poll"

	// Kind "share": URL, Name (title), Text (description), Host.
	// Kind "contact": Name, Phone. Kind "sticker": URL (static PNG), LottieURL
	// (Telegram-compatible .tgs when set), Text (emoji tags).
	// Kind "keyboard": Text (buttons rendered as lines).
	Host      string
	Phone     string
	LottieURL string

	// Kind "location": Lat, Lon. Kind "contact": ContactID when MAX sent only a
	// user reference (Phone empty). Kind "keyboard": Buttons + CallbackID (Text
	// keeps the plain-text rendering as a fallback).
	Lat, Lon   float64
	ContactID  int64
	CallbackID string
	Buttons    [][]KeyButton

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

// KeyButton is one button of a MAX bot's inline keyboard.
type KeyButton struct {
	Type    string // LINK | CALLBACK | CLIPBOARD | MESSAGE | OPEN_APP | REQUEST_GEO_LOCATION | REQUEST_CONTACT | ...
	Text    string
	URL     string
	Payload string
	WebApp  string
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
	Time        int64  // message timestamp in ms since epoch, as MAX reports it (0 if unknown)

	// ChatClosed marks the service push MAX sends when a chat is deleted
	// ("Чат закрыт"); only ChatID is filled in then.
	ChatClosed bool
}

// attach is a message attachment as MAX sends it. PHOTO exposes a public webp
// image at baseUrl; FILE/VIDEO expose fileUrl/url. Voice notes (sent as _type
// AUDIO, or UNSUPPORTED when the server decides our WEB device type can't
// render them) carry duration+audioId+token instead of a fetchable URL. Polls
// only arrive in full over the binary transport (see binproto.go); over JSON
// the server sends a bare UNSUPPORTED instead.
type attach struct {
	Type      string      `json:"_type"`
	BaseURL   string      `json:"baseUrl"`
	FileURL   string      `json:"fileUrl"`
	URL       string      `json:"url"`
	Name      string      `json:"name"`
	Duration  int64       `json:"duration"` // ms, present on voice/audio attaches
	FileID    json.Number `json:"fileId"`   // FILE attaches: internal id, pairs with Token
	VideoID   json.Number `json:"videoId"`  // VIDEO attaches: internal id, pairs with Token
	AudioID   json.Number `json:"audioId"`  // AUDIO/UNSUPPORTED (voice) attaches: internal id, pairs with Token
	Token     string      `json:"token"`    // internal token that pairs with fileId/videoId/audioId
	Width     int         `json:"width"`    // VIDEO attaches: size as shown (rotation applied)
	Height    int         `json:"height"`
	VideoType int         `json:"videoType"` // VIDEO attaches: 1 = round video message

	// POLL attaches (confirmed from live binary frames):
	// {pollId, title, answers:[{answerId,text}], state:{result:[{answerId,voteCount,...}], total}}
	Title   string `json:"title"`
	Answers []struct {
		AnswerID int64  `json:"answerId"`
		Text     string `json:"text"`
	} `json:"answers"`
	PollID json.Number `json:"pollId"`
	State  pollState   `json:"state"`

	// SHARE (link preview): {url, title, description, host, image}.
	Description string `json:"description"`
	Host        string `json:"host"`
	// CONTACT: {contactId, name, firstName, lastName, phone, vcfBody, photoUrl}.
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Phone     string `json:"phone"`
	// STICKER: {url (static PNG), lottieUrl (gzipped Lottie = Telegram .tgs), tags}.
	LottieURL string   `json:"lottieUrl"`
	Tags      []string `json:"tags"`
	// INLINE_KEYBOARD (bot buttons): {callbackId, keyboard:{buttons:[[{type,text,url,payload}]]}}.
	CallbackID string `json:"callbackId"` // shared by the CALLBACK buttons of this keyboard
	Keyboard   struct {
		Buttons [][]struct {
			Type    string `json:"type"` // LINK | CALLBACK | CLIPBOARD | MESSAGE | OPEN_APP | REQUEST_GEO_LOCATION | REQUEST_CONTACT
			Text    string `json:"text"`
			URL     string `json:"url"`
			Payload string `json:"payload"`
			WebApp  string `json:"webApp"` // OPEN_APP: the mini-app's bot name
		} `json:"buttons"`
	} `json:"keyboard"`

	// LOCATION: {latitude, longitude, zoom}. CONTACT may instead carry just
	// contactId (a MAX user reference, no phone — needs a lookup).
	Latitude  float64     `json:"latitude"`
	Longitude float64     `json:"longitude"`
	ContactID json.Number `json:"contactId"`

	// CONTROL: service events — {event:"system", message:"Чат закрыт"} when a
	// chat is deleted; "new"/"join"/"leave"/"title" for group changes.
	Event        string `json:"event"`
	ControlText  string `json:"message"`
	ShortMessage string `json:"shortMessage"`

	raw json.RawMessage // the attach exactly as received, for logging unknown shapes
}

// pollState is a POLL's "state" object: per-answer results plus the total.
// options&1 is set on the answer(s) the current user voted for (seen live:
// options=1 exactly on the answer our own userId appears under).
type pollState struct {
	Result []struct {
		AnswerID  int64 `json:"answerId"`
		VoteCount int   `json:"voteCount"`
		Options   int   `json:"options"`
	} `json:"result"`
	Total int `json:"total"`
}

func (s pollState) toPollState(pollID int64) PollState {
	ps := PollState{PollID: pollID, Total: s.Total, Votes: map[int64]int{}, Mine: map[int64]bool{}}
	for _, r := range s.Result {
		ps.Votes[r.AnswerID] = r.VoteCount
		if r.Options&1 != 0 {
			ps.Mine[r.AnswerID] = true
		}
	}
	return ps
}

// PollAnswer is one option of a MAX poll.
type PollAnswer struct {
	ID    int64  `json:"id"`
	Text  string `json:"text"`
	Votes int    `json:"votes"`
	Mine  bool   `json:"mine"` // the bridged MAX account voted for this answer
}

// Poll is a normalized MAX poll (POLL attach).
type Poll struct {
	ID      int64        `json:"id"`
	Title   string       `json:"title"`
	Answers []PollAnswer `json:"answers"`
	Total   int          `json:"total"`
}

// PollState is a poll's current results, as returned by poll refreshes/votes.
type PollState struct {
	PollID int64
	Total  int
	Votes  map[int64]int  // answerId -> vote count
	Mine   map[int64]bool // answerIds the bridged account voted for
}

// Apply copies fresh results into p (answer texts are kept as they were).
func (p *Poll) Apply(s PollState) {
	p.Total = s.Total
	for i := range p.Answers {
		p.Answers[i].Votes = s.Votes[p.Answers[i].ID]
		p.Answers[i].Mine = s.Mine[p.Answers[i].ID]
	}
}

func pollFromAttach(a attach) *Poll {
	id := numToInt64(a.PollID)
	st := a.State.toPollState(id)
	p := &Poll{ID: id, Title: a.Title}
	for _, ans := range a.Answers {
		p.Answers = append(p.Answers, PollAnswer{ID: ans.AnswerID, Text: ans.Text})
	}
	p.Apply(st)
	return p
}

// UnmarshalJSON keeps the raw bytes alongside the parsed fields so unknown
// attach types can be logged as received.
func (a *attach) UnmarshalJSON(data []byte) error {
	type plain attach
	if err := json.Unmarshal(data, (*plain)(a)); err != nil {
		return err
	}
	a.raw = append(json.RawMessage(nil), data...)
	return nil
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
	// Status is absent for a new message; a repeat push of the same id carries
	// "EDITED" or "REMOVED" (how MAX really signals deletions — per Telemax).
	Status string `json:"status"`
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
			att := Attachment{Kind: "video", URL: u, Name: a.Name, Width: a.Width, Height: a.Height, Round: a.VideoType == 1}
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
			}
			if u == "" && att.ResolveKind == "" && a.Duration == 0 {
				log.Printf("[max] bare %s attach (no duration/audioId): %s", a.Type, a.raw)
				out = append(out, Attachment{Kind: "other", Name: a.Type})
			} else {
				out = append(out, att)
			}
		case "POLL":
			out = append(out, Attachment{Kind: "poll", Text: a.Title, Poll: pollFromAttach(a)})
		case "SHARE":
			if a.URL == "" {
				logAttachOnce(a)
				continue
			}
			out = append(out, Attachment{Kind: "share", URL: a.URL, Name: a.Title, Text: a.Description, Host: a.Host})
		case "CONTACT":
			name := strings.TrimSpace(a.FirstName + " " + a.LastName)
			if name == "" {
				name = a.Name
			}
			out = append(out, Attachment{Kind: "contact", Name: name, Phone: a.Phone, ContactID: numToInt64(a.ContactID)})
		case "LOCATION":
			out = append(out, Attachment{Kind: "location", Lat: a.Latitude, Lon: a.Longitude})
		case "STICKER":
			out = append(out, Attachment{Kind: "sticker", URL: a.URL, LottieURL: a.LottieURL, Text: strings.Join(a.Tags, "")})
		case "INLINE_KEYBOARD":
			var lines []string
			var rows [][]KeyButton
			for _, row := range a.Keyboard.Buttons {
				var r []KeyButton
				for _, b := range row {
					if b.URL != "" {
						lines = append(lines, "🔗 "+b.Text+": "+b.URL)
					} else if b.Text != "" {
						lines = append(lines, "🔘 "+b.Text)
					}
					r = append(r, KeyButton{Type: b.Type, Text: b.Text, URL: b.URL, Payload: b.Payload, WebApp: b.WebApp})
				}
				if len(r) > 0 {
					rows = append(rows, r)
				}
			}
			if len(lines) > 0 {
				out = append(out, Attachment{Kind: "keyboard", Text: strings.Join(lines, "\n"), Buttons: rows, CallbackID: a.CallbackID})
			}
		case "CONTROL":
			// Service-only markers (e.g. "bot started") — not user content.
		default:
			logAttachOnce(a)
			out = append(out, Attachment{Kind: "other", Name: a.Type})
		}
	}
	return out
}

// isChatClosed reports whether the attaches hold MAX's "chat deleted" service
// marker: a CONTROL attach with event "system" and the text "Чат закрыт"
// (seen live by Telemax — it is NOT a CHAT_UPDATE status).
func isChatClosed(as []attach) bool {
	for _, a := range as {
		if a.Type != "CONTROL" || a.Event != "system" {
			continue
		}
		t := a.ControlText
		if t == "" {
			t = a.ShortMessage
		}
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(t), "."), "чат закрыт") {
			return true
		}
	}
	return false
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

// unknownAttachLogged keeps "unknown attach type" to one sample per type per
// run: history resyncs re-parse the same messages on every reconnect, which
// otherwise floods the log with identical lines.
var unknownAttachLogged sync.Map

func logAttachOnce(a attach) {
	if _, seen := unknownAttachLogged.LoadOrStore(a.Type, true); seen {
		return
	}
	s := string(a.raw)
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	log.Printf("[max] unknown attach type %q (shown once per run): %s", a.Type, s)
}
