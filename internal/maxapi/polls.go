package maxapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// PollRef identifies one poll message for a refresh request.
type PollRef struct {
	MessageID string
	PollID    int64
}

// pollReplySample makes the first reply of each poll opcode get logged in
// full (truncated), so its real shape is visible in the service log.
var pollReplySample sync.Map // opcode -> struct{}

func logPollReplyOnce(opcode int, payload []byte) {
	if _, seen := pollReplySample.LoadOrStore(opcode, struct{}{}); seen {
		return
	}
	s := string(payload)
	if len(s) > 1500 {
		s = s[:1500] + "…"
	}
	log.Printf("[poll] first reply to opcode %d (sample, for reference): %s", opcode, s)
}

// PollStates fetches current results for polls in one chat via opcode 306 —
// the same request web.max.ru repeats every few seconds while a poll is on
// screen. The reply shape is not documented, so it is parsed generically:
// any object carrying a pollId plus a state (or a bare result list) counts.
func (c *Client) PollStates(chatID int64, refs []PollRef) (map[int64]PollState, error) {
	polls := make([]map[string]any, 0, len(refs))
	for _, r := range refs {
		polls = append(polls, map[string]any{"messageId": msgIDArg(r.MessageID), "pollId": r.PollID})
	}
	resp, err := c.request(OpPollUpdates, map[string]any{"chatId": chatID, "polls": polls}, 15*time.Second)
	if err != nil {
		return nil, err
	}
	logPollReplyOnce(OpPollUpdates, resp.Payload)
	return findPollStates(resp.Payload), nil
}

// Vote casts the bridged account's vote in a poll via SEND_VOTE (opcode 304).
// Request: {chatId, messageId, pollId, answersIds} — field set mirrors the
// confirmed opcode-306 request; the "answersIds" spelling comes from the
// server's own validation error ("Field requirement failed: answersIds").
// Note: a rejected vote made the server close the session (1000) right after. Returns the fresh poll state when the reply
// carries one.
func (c *Client) Vote(chatID int64, messageID string, pollID int64, answerIDs []int64) (*PollState, error) {
	req := map[string]any{
		"chatId":     chatID,
		"messageId":  msgIDArg(messageID),
		"pollId":     pollID,
		"answersIds": answerIDs, // sic — confirmed by the server's validation error
	}
	log.Printf("[poll] SEND_VOTE chat=%d msg=%s poll=%d answers=%v", chatID, messageID, pollID, answerIDs)
	resp, err := c.request(OpSendVote, req, 15*time.Second)
	if err != nil {
		return nil, err
	}
	logPollReplyOnce(OpSendVote, resp.Payload)
	if st, ok := findPollStates(resp.Payload)[pollID]; ok {
		return &st, nil
	}
	return nil, nil
}

// findPollStates walks an arbitrary JSON payload and collects the state of
// every poll object it finds (keyed by pollId).
func findPollStates(payload []byte) map[int64]PollState {
	var root any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if dec.Decode(&root) != nil {
		return nil
	}
	out := map[int64]PollState{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			if id, ok := x["pollId"].(json.Number); ok {
				if st, ok := stateFrom(x); ok {
					pid, _ := id.Int64()
					out[pid] = st.toPollState(pid)
					return
				}
			}
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(root)
	return out
}

// stateFrom extracts a pollState from a poll-ish object: either its "state"
// field or, failing that, the object itself if it has a "result" list.
func stateFrom(m map[string]any) (pollState, bool) {
	src, ok := m["state"]
	if !ok {
		if _, has := m["result"]; !has {
			return pollState{}, false
		}
		src = m
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return pollState{}, false
	}
	var st pollState
	if json.Unmarshal(raw, &st) != nil || st.Result == nil {
		return pollState{}, false
	}
	return st, true
}

// FormatPollState renders a PollState compactly for logs.
func FormatPollState(s PollState) string {
	return fmt.Sprintf("total=%d votes=%v mine=%v", s.Total, s.Votes, s.Mine)
}
