package control

import (
	"encoding/json"
	"testing"

	"github.com/thupham/hive/internal/permission"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// The recorded decision follows the option the user chose, not a flag the
// transport guessed.
//
// Found in a live conversation: an agent's options are named "allow_once" and
// "allow_always", the transport read the kind against "allow", and every approval
// was recorded as a denial — the audit trail said the opposite of what the user
// did.
func TestTheRecordedDecisionFollowsTheChosenOption(t *testing.T) {
	request := &permission.Request{Payload: json.RawMessage(`{
		"toolCall": {"title": "Run git"},
		"options": [
			{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"},
			{"optionId": "allow_always", "name": "Always", "kind": "allow_always"},
			{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"}
		]
	}`)}

	for _, tc := range []struct {
		name   string
		params v1.PermissionRespondParams
		want   permission.State
	}{
		{"an option that allows", v1.PermissionRespondParams{OptionID: "allow_once"}, permission.StateApproved},
		{"an option that allows always", v1.PermissionRespondParams{OptionID: "allow_always"}, permission.StateApproved},
		{"an option that rejects", v1.PermissionRespondParams{OptionID: "reject_once"}, permission.StateDenied},
		{"no option, approved", v1.PermissionRespondParams{Approved: true}, permission.StateApproved},
		{"no option, denied", v1.PermissionRespondParams{}, permission.StateDenied},
		{"an option nobody offered", v1.PermissionRespondParams{OptionID: "not_there"}, permission.StateDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionState(request, tc.params); got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

// A request whose payload cannot be read falls back to the flag rather than
// failing.
func TestTheRecordedDecisionSurvivesAnUnreadablePayload(t *testing.T) {
	request := &permission.Request{Payload: json.RawMessage(`not json`)}

	if got := decisionState(request, v1.PermissionRespondParams{OptionID: "allow_once"}); got != permission.StateDenied {
		t.Errorf("state = %q, want denied", got)
	}
	if got := decisionState(request, v1.PermissionRespondParams{Approved: true}); got != permission.StateApproved {
		t.Errorf("state = %q, want approved", got)
	}
}
