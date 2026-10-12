package maxapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Account, group and bot-button operations. Opcode numbers and payload shapes
// were taken from the open-source Telemax bridge (github.com/Trollobot/Telemax,
// AGPL-3.0), which verified them against live MAX; only those wire facts are
// reused — the implementation below is our own.

// Contact is a MAX user (or public bot) as returned by the lookup opcodes.
type Contact struct {
	ID          int64
	Name        string
	Phone       string // digits, no "+" (empty when the user hides it)
	Link        string // https://max.ru/<nick> for bots
	Description string
}

func contactFrom(ci contactInfo) Contact {
	return Contact{
		ID:          numToInt64(ci.ID),
		Name:        pickName(ci.Names),
		Phone:       strings.TrimPrefix(rawText(ci.Phone), "+"),
		Link:        ci.Link,
		Description: ci.Description,
	}
}

// FindByPhone looks a MAX user up by phone number. (nil, nil) means the number
// is not registered in MAX.
func (c *Client) FindByPhone(phone string) (*Contact, error) {
	resp, err := c.request(OpFindByPhone, map[string]any{"phone": strings.TrimPrefix(phone, "+")}, 15*time.Second)
	if err != nil {
		if strings.Contains(err.Error(), "not.found") {
			return nil, nil
		}
		return nil, err
	}
	var r struct {
		Contact *contactInfo `json:"contact"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil || r.Contact == nil {
		return nil, fmt.Errorf("unexpected FIND_BY_PHONE answer: %s", truncate(string(resp.Payload), 200))
	}
	ct := contactFrom(*r.Contact)
	return &ct, nil
}

// ContactByID returns a user's profile (nil, nil when MAX knows no such id).
func (c *Client) ContactByID(id int64) (*Contact, error) {
	resp, err := c.request(OpGetContacts, map[string]any{"contactIds": []int64{id}}, 15*time.Second)
	if err != nil {
		return nil, err
	}
	var r struct {
		Contacts []contactInfo `json:"contacts"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil {
		return nil, fmt.Errorf("unexpected CONTACT_INFO answer: %s", truncate(string(resp.Payload), 200))
	}
	for _, ci := range r.Contacts {
		if numToInt64(ci.ID) == id {
			ct := contactFrom(ci)
			return &ct, nil
		}
	}
	return nil, nil
}

// ContactPhone resolves a user's phone number ("" when unknown or hidden).
func (c *Client) ContactPhone(id int64) string {
	ct, err := c.ContactByID(id)
	if err != nil || ct == nil {
		return ""
	}
	return ct.Phone
}

// Channel is a public channel found by PublicSearch.
type Channel struct {
	ID    int64
	Title string
	Link  string
}

// PublicSearch searches MAX's public catalogue by title or @nick. It returns
// bots and channels — ordinary people are not searchable this way.
func (c *Client) PublicSearch(query string) ([]Contact, []Channel, error) {
	query = strings.TrimPrefix(strings.TrimSpace(query), "@") // a leading @ finds nothing
	resp, err := c.request(OpPublicSearch, map[string]any{"query": query, "count": 10}, 15*time.Second)
	if err != nil {
		return nil, nil, err
	}
	// Each hit is {chat:{id,type,title,link}} or {contact:{contact:{profile}, summary}}
	// (the profile is nested twice; a single nesting is accepted too).
	var r struct {
		Result []struct {
			Contact *struct {
				contactInfo
				Inner *contactInfo `json:"contact"`
			} `json:"contact"`
			Chat *struct {
				ID    json.Number `json:"id"`
				Title string      `json:"title"`
				Link  string      `json:"link"`
			} `json:"chat"`
		} `json:"result"`
		Contacts []contactInfo `json:"contacts"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil {
		return nil, nil, fmt.Errorf("unexpected PUBLIC_SEARCH answer: %s", truncate(string(resp.Payload), 200))
	}
	var contacts []Contact
	var channels []Channel
	for _, ci := range r.Contacts {
		contacts = append(contacts, contactFrom(ci))
	}
	for _, it := range r.Result {
		if it.Contact != nil {
			ci := it.Contact.contactInfo
			if it.Contact.Inner != nil {
				ci = *it.Contact.Inner
			}
			if ci.ID != "" {
				contacts = append(contacts, contactFrom(ci))
			}
		}
		if it.Chat != nil && it.Chat.ID != "" && it.Chat.Title != "" {
			channels = append(channels, Channel{ID: numToInt64(it.Chat.ID), Title: it.Chat.Title, Link: it.Chat.Link})
		}
	}
	return contacts, channels, nil
}

// SendLocation posts a map point as a LOCATION attach.
func (c *Client) SendLocation(chatID int64, lat, lon float64, replyToMaxID string) (string, error) {
	return c.sendAttach(chatID, nil, replyToMaxID, map[string]any{
		"_type": "LOCATION", "latitude": lat, "longitude": lon, "zoom": 14,
	})
}

// SendContact posts a phone contact as a self-contained vCard-style CONTACT attach.
func (c *Client) SendContact(chatID int64, firstName, lastName, phone, replyToMaxID string) (string, error) {
	full := strings.TrimSpace(firstName + " " + lastName)
	vcf := fmt.Sprintf("BEGIN:VCARD\r\nVERSION:2.1\r\nN:%s;%s;;;\r\nFN:%s\r\nTEL;CELL:%s\r\nEND:VCARD\r\n", lastName, firstName, full, phone)
	return c.sendAttach(chatID, nil, replyToMaxID, map[string]any{
		"_type": "CONTACT", "firstName": firstName, "lastName": lastName,
		"phone": phone, "vcfBody": vcf, "name": firstName,
	})
}

// SendToNewDialog opens a 1:1 dialog with a user we have no chat with yet by
// sending the first message with a top-level userId (how the official app's
// "open chat" works). Returns the new dialog's chat id and the message id.
func (c *Client) SendToNewDialog(userID int64, text string) (chatID int64, msgID string, err error) {
	if strings.TrimSpace(text) == "" {
		return 0, "", fmt.Errorf("the first message of a new chat can't be empty")
	}
	resp, err := c.request(OpMsgSend, map[string]any{
		"userId": userID,
		"message": map[string]any{
			"text": text, "cid": time.Now().UnixNano(), "elements": []any{}, "attaches": []any{},
		},
		"notify": true,
	}, 20*time.Second)
	if err != nil {
		return 0, "", err
	}
	var r struct {
		ChatID  json.Number `json:"chatId"`
		Message struct {
			ID json.Number `json:"id"`
		} `json:"message"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil || numToInt64(r.ChatID) == 0 {
		return 0, "", fmt.Errorf("MAX did not return a chat id: %s", truncate(string(resp.Payload), 200))
	}
	return numToInt64(r.ChatID), r.Message.ID.String(), nil
}

// CreateGroup creates a group chat (a MSG_SEND with a CONTROL "new" attach and
// no chatId) and invites userIDs right away. Returns the new chat's id.
func (c *Client) CreateGroup(title string, userIDs []int64) (int64, error) {
	if userIDs == nil {
		userIDs = []int64{}
	}
	resp, err := c.request(OpMsgSend, map[string]any{
		"message": map[string]any{
			"cid": time.Now().UnixNano(),
			"attaches": []map[string]any{{
				"_type": "CONTROL", "event": "new", "chatType": "CHAT",
				"title": title, "userIds": userIDs,
			}},
		},
		"notify": true,
	}, 25*time.Second)
	if err != nil {
		return 0, err
	}
	var r struct {
		Chat struct {
			ID json.Number `json:"id"`
		} `json:"chat"`
		ChatID json.Number `json:"chatId"`
	}
	if json.Unmarshal(resp.Payload, &r) == nil {
		if id := numToInt64(r.Chat.ID); id != 0 {
			return id, nil
		}
		if id := numToInt64(r.ChatID); id != 0 {
			return id, nil
		}
	}
	return 0, fmt.Errorf("MAX did not return the new chat's id: %s", truncate(string(resp.Payload), 200))
}

// AddMembers invites users to a group chat.
func (c *Client) AddMembers(chatID int64, userIDs []int64) error {
	return c.chatMembers(chatID, userIDs, "add")
}

// RemoveMembers removes users from a group chat.
func (c *Client) RemoveMembers(chatID int64, userIDs []int64) error {
	return c.chatMembers(chatID, userIDs, "remove")
}

func (c *Client) chatMembers(chatID int64, userIDs []int64, op string) error {
	_, err := c.request(OpChatMembers, map[string]any{
		"chatId": chatID, "userIds": userIDs, "showHistory": true, "operation": op,
	}, 20*time.Second)
	return err
}

// SetChatInfo renames a group and/or changes its description (nil = unchanged).
func (c *Client) SetChatInfo(chatID int64, title, description *string) error {
	p := map[string]any{"chatId": chatID}
	if title != nil {
		p["title"] = *title
	}
	if description != nil {
		p["description"] = *description
	}
	_, err := c.request(OpChatSetInfo, p, 15*time.Second)
	if err == nil && title != nil {
		c.chatMu.Lock()
		c.chatName[chatID] = *title
		c.chatMu.Unlock()
	}
	return err
}

// LeaveChat leaves a group chat.
func (c *Client) LeaveChat(chatID int64) error {
	_, err := c.request(OpChatLeave, map[string]any{"chatId": chatID}, 15*time.Second)
	return err
}

// DeleteChat deletes a chat from our account; forAll also deletes it for every
// participant (irreversible for them too).
func (c *Client) DeleteChat(chatID int64, forAll bool) error {
	_, err := c.request(OpChatDelete, map[string]any{
		"chatId": chatID, "lastEventTime": time.Now().UnixMilli(), "forAll": forAll,
	}, 15*time.Second)
	return err
}

// PressButton presses a CALLBACK button of a bot's inline keyboard.
func (c *Client) PressButton(chatID int64, callbackID, payload string) error {
	p := map[string]any{"callbackId": callbackID, "chatId": chatID}
	if payload != "" {
		p["payload"] = payload
	}
	_, err := c.request(OpMsgCallback, p, 15*time.Second)
	return err
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
