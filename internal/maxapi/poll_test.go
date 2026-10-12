package maxapi

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Shape confirmed from a live binary GET_HISTORY reply.
const pollAttachJSON = `{"_type":"POLL","pollId":10479875,"title":"Обедают?","settings":4,"version":1,
	"answers":[{"answerId":1,"text":"Да"},{"answerId":2,"text":"Нет"}],
	"state":{"total":1,"voterPreviewIds":[1],"result":[
		{"answerId":1,"voteCount":1,"rate":100,"options":1,"votes":[{"userId":1,"timestamp":2}]},
		{"answerId":2,"voteCount":0,"rate":0,"options":0,"votes":[]}]}}`

func TestPollAttach(t *testing.T) {
	var a attach
	if err := json.Unmarshal([]byte(pollAttachJSON), &a); err != nil {
		t.Fatal(err)
	}
	got := toAttachments([]attach{a})
	if len(got) != 1 || got[0].Kind != "poll" || got[0].Poll == nil {
		t.Fatalf("unexpected: %+v", got)
	}
	want := &Poll{ID: 10479875, Title: "Обедают?", Total: 1, Answers: []PollAnswer{
		{ID: 1, Text: "Да", Votes: 1, Mine: true},
		{ID: 2, Text: "Нет", Votes: 0},
	}}
	if !reflect.DeepEqual(got[0].Poll, want) {
		t.Fatalf("poll = %+v, want %+v", got[0].Poll, want)
	}
}

func TestFindPollStates(t *testing.T) {
	// Both a full poll object and a bare {pollId, result, total} must parse.
	payload := `{"chatId":1,"polls":[` + pollAttachJSON + `,
		{"pollId":7,"total":3,"result":[{"answerId":2,"voteCount":3}]}]}`
	got := findPollStates([]byte(payload))
	if len(got) != 2 {
		t.Fatalf("got %d states: %+v", len(got), got)
	}
	if s := got[10479875]; s.Total != 1 || s.Votes[1] != 1 || !s.Mine[1] {
		t.Fatalf("poll 10479875: %+v", s)
	}
	if s := got[7]; s.Total != 3 || s.Votes[2] != 3 {
		t.Fatalf("poll 7: %+v", s)
	}
}
