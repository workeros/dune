package fabricd

import (
	"encoding/json"
	"fmt"

	"github.com/aiomni/dune/pkg/api"
)

type acpConfigOption struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Options []struct {
		Value   *string `json:"value"`
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	} `json:"options"`
}

func (a *acpController) configurationOptionsLocked() ([]acpConfigOption, bool) {
	conversation := a.conversation.describe()
	if conversation == nil {
		return nil, false
	}
	var state struct {
		Options []acpConfigOption `json:"configOptions"`
	}
	raw, exists := conversation.State["config_option_update"]
	if !exists {
		return nil, false
	}
	if json.Unmarshal(raw, &state) != nil {
		return nil, true
	}
	return state.Options, true
}

func (a *acpController) validateConfigurationLocked(req api.ACPAction) error {
	if err := a.checkPromptConversationLocked(req); err != nil {
		return err
	}
	if a.active != nil || len(a.queue) > 0 {
		return &api.Error{Code: "BUSY", Detail: "change configuration when the current operation finishes"}
	}
	if req.SessionID == "" || req.SessionID != a.state.SessionID || req.Cwd != a.state.Cwd {
		return &api.Error{Code: "STALE_SESSION", Detail: "native ACP session changed"}
	}
	options, hasConfig := a.configurationOptionsLocked()
	if req.Action == "set_mode" {
		if hasConfig {
			return fmt.Errorf("this Agent exposes modes through config options")
		}
		var modes struct {
			Available []struct {
				ID string `json:"id"`
			} `json:"availableModes"`
		}
		_ = json.Unmarshal(a.conversation.describe().State["modes"], &modes)
		for _, mode := range modes.Available {
			if mode.ID == req.ModeID && mode.ID != "" {
				return nil
			}
		}
		return fmt.Errorf("Agent did not advertise this mode")
	}
	if len(req.ConfigValue) > 8192 {
		return fmt.Errorf("configuration value too large")
	}
	for _, option := range options {
		if option.ID != req.ConfigID {
			continue
		}
		if option.Type == "boolean" {
			if string(req.ConfigValue) == "true" || string(req.ConfigValue) == "false" {
				return nil
			}
		} else if option.Type == "select" {
			var value string
			if json.Unmarshal(req.ConfigValue, &value) != nil {
				break
			}
			for _, item := range option.Options {
				if item.Value != nil && *item.Value == value {
					return nil
				}
				for _, nested := range item.Options {
					if nested.Value == value {
						return nil
					}
				}
			}
		}
		break
	}
	return fmt.Errorf("Agent did not advertise this configuration option/value")
}

func (a *acpController) configurationParamsLocked(req api.ACPAction) map[string]any {
	if req.Action == "set_mode" {
		return map[string]any{"sessionId": req.SessionID, "modeId": req.ModeID}
	}
	params := map[string]any{"sessionId": req.SessionID, "configId": req.ConfigID, "value": req.ConfigValue}
	options, _ := a.configurationOptionsLocked()
	for _, option := range options {
		if option.ID == req.ConfigID {
			params["type"] = option.Type
			break
		}
	}
	return params
}

func (a *acpController) retainSessionConfigurationLocked(result json.RawMessage) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil {
		return
	}
	a.conversation.mutate(func(model *conversationModel) {
		if config, exists := fields["configOptions"]; exists && string(config) != "null" {
			model.setState("config_option_update", api.Payload(map[string]any{"configOptions": config}))
		}
		if modes, exists := fields["modes"]; exists && string(modes) != "null" {
			model.setState("modes", modes)
		}
	})
}

func (a *acpController) applyConfigurationResultLocked(req api.ACPAction, result json.RawMessage) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil || fields == nil {
		return &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent returned invalid configuration result"}
	}
	if req.Action == "set_config_option" {
		var options []json.RawMessage
		if json.Unmarshal(fields["configOptions"], &options) != nil || options == nil {
			return &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent did not confirm complete configuration state"}
		}
		a.retainSessionConfigurationLocked(result)
	} else {
		a.conversation.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("current_mode_update"), "currentModeId": api.Payload(req.ModeID)}, "")
	}
	return nil
}
