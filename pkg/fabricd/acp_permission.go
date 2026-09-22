package fabricd

import (
	"encoding/json"

	"github.com/aiomni/dune/pkg/api"
)

func (a *acpController) permissionRecordLocked(permission acpPermission, state, response string) {
	var params struct {
		Title    string `json:"title"`
		ToolCall struct {
			ID    string `json:"toolCallId"`
			Title string `json:"title"`
		} `json:"toolCall"`
	}
	_ = json.Unmarshal(permission.Params, &params)
	title := params.Title
	if title == "" {
		title = params.ToolCall.Title
	}
	if title == "" {
		title = "Agent 操作"
	}
	record := api.ACPInteractionRecord{ID: permission.ID, Kind: "permission", State: state, Title: title, ToolCallID: params.ToolCall.ID, Response: response}
	a.conversation.mutate(func(model *conversationModel) {
		if model.description.ID != permission.ConversationID {
			return
		}
		key := "interaction\x00" + permission.ID
		entry := model.find(key)
		if entry == nil {
			entry = &api.ACPEntry{Type: "activity", TurnID: permission.TurnID, ContextIncomplete: state != "pending"}
		}
		entry.Activity = &api.ACPActivity{UpdateType: "interaction", Data: api.Payload(record)}
		model.put(*entry, key)
	})
}

func (a *acpController) clearPermissionsLocked(state string) {
	for id, permission := range a.permissions {
		a.permissionRecordLocked(permission, state, "")
		if a.releaseControl != nil {
			a.releaseControl("permission", id)
		}
	}
	a.permissions = map[string]acpPermission{}
}
