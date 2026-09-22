package fabricd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/api"
)

func promptContent(req api.ACPAction) []json.RawMessage {
	content := make([]json.RawMessage, 0, len(req.Attachments)+1)
	if req.Text != "" {
		content = append(content, api.Payload(map[string]string{"type": "text", "text": req.Text}))
	}
	return append(content, req.Attachments...)
}

func (a *acpController) redactedPromptContent(req api.ACPAction) []json.RawMessage {
	var content []json.RawMessage
	_ = json.Unmarshal(a.redactCredential(api.Payload(promptContent(req))), &content)
	return content
}

func (a *acpController) validatePromptLocked(req api.ACPAction) error {
	if req.SessionID == "" || len(req.Text) > 64*1024 || !utf8.ValidString(req.Text) || req.Text == "" && len(req.Attachments) == 0 {
		return fmt.Errorf("create/load a session and supply text or supported attachments; text is limited to 64 KiB")
	}
	if len(req.Attachments) > 8 || len(api.Payload(promptContent(req))) > api.MaxACPPromptBytes {
		return fmt.Errorf("prompt exceeds 8 attachments or 2 MiB")
	}
	for _, raw := range req.Attachments {
		var block struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MIME     string `json:"mimeType"`
			URI      string `json:"uri"`
			Name     string `json:"name"`
			Resource *struct {
				URI  string  `json:"uri"`
				Text *string `json:"text"`
				Blob *string `json:"blob"`
			} `json:"resource"`
		}
		if json.Unmarshal(raw, &block) != nil {
			return fmt.Errorf("invalid attachment")
		}
		supported := false
		switch block.Type {
		case "image", "audio":
			supported = (block.Type == "image" && a.state.PromptCapabilities.Image || block.Type == "audio" && a.state.PromptCapabilities.Audio) && strings.HasPrefix(block.MIME, block.Type+"/") && validBase64(block.Data)
		case "resource_link":
			supported = validResourceURI(block.URI) && block.Name != ""
		case "resource":
			resource := block.Resource
			supported = a.state.PromptCapabilities.EmbeddedContext && resource != nil && validResourceURI(resource.URI)
			if supported {
				supported = (resource.Text != nil && resource.Blob == nil && utf8.ValidString(*resource.Text)) || (resource.Text == nil && resource.Blob != nil && validBase64(*resource.Blob))
			}
		}
		if !supported {
			return &api.Error{Code: "UNSUPPORTED", Detail: "invalid attachment or Agent did not advertise its prompt capability"}
		}
	}
	return nil
}

func validBase64(value string) bool {
	_, err := base64.StdEncoding.DecodeString(value)
	return err == nil
}
func validResourceURI(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs()
}
