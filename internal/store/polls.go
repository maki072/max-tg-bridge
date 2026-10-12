package store

import "maxtg/internal/maxapi"

// PollRec ties a MAX poll to the Telegram message that mirrors it, so inline
// "vote" buttons and periodic result refreshes can find their way back.
type PollRec struct {
	MaxChatID string `json:"max_chat_id"`
	MaxMsgID  string `json:"max_msg_id"`
	TgMsgID   int64  `json:"tg_msg_id"`
	// Native polls: the Telegram poll id (for poll_answer updates) and the
	// separate "results in MAX" message that gets edited. For button-style
	// polls TgPollID is empty and ResultsMsgID == TgMsgID.
	TgPollID     string      `json:"tg_poll_id,omitempty"`
	ResultsMsgID int64       `json:"results_msg_id,omitempty"`
	Header       string      `json:"header"`  // "sender name" line the message was posted under
	Created      int64       `json:"created"` // unix seconds the poll was mirrored
	Poll         maxapi.Poll `json:"poll"`
}

const maxPolls = 200

// SavePoll inserts or replaces (by Telegram message id) a poll record.
func (s *Store) SavePoll(r PollRec) {
	if r.TgMsgID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Polls {
		if s.Polls[i].TgMsgID == r.TgMsgID {
			s.Polls[i] = r.clone()
			_ = s.save()
			return
		}
	}
	s.Polls = append(s.Polls, r.clone())
	if len(s.Polls) > maxPolls {
		s.Polls = s.Polls[len(s.Polls)-maxPolls:]
	}
	_ = s.save()
}

// PollByTg returns the poll mirrored by a Telegram message (the poll itself
// or its results message).
func (s *Store) PollByTg(tgMsgID int64) (PollRec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.Polls {
		if r.TgMsgID == tgMsgID || r.ResultsMsgID == tgMsgID {
			return r.clone(), true
		}
	}
	return PollRec{}, false
}

// PollByTgPollID returns the poll mirrored as the given native Telegram poll.
func (s *Store) PollByTgPollID(pollID string) (PollRec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.Polls {
		if pollID != "" && r.TgPollID == pollID {
			return r.clone(), true
		}
	}
	return PollRec{}, false
}

// PollsSince returns a snapshot of poll records mirrored at or after the given
// unix time (the ones still worth refreshing).
func (s *Store) PollsSince(unix int64) []PollRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PollRec
	for _, r := range s.Polls {
		if r.Created >= unix {
			out = append(out, r.clone())
		}
	}
	return out
}

// clone deep-copies the answers slice so callers can mutate the result
// (e.g. Poll.Apply) without racing the stored record.
func (r PollRec) clone() PollRec {
	r.Poll.Answers = append([]maxapi.PollAnswer(nil), r.Poll.Answers...)
	return r
}
