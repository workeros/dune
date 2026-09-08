package lifecycle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/runner"
)

// CreateSpec is the immutable, public template selection for one creation.
// The lifecycle service must validate the selected template's field types,
// ranges and allowlist before persistence. Private provider configuration and
// secrets never belong here. Parameters are bounded scalar JSON values; their
// meaning belongs to this exact template version, not the current template.
type CreateSpec struct {
	Name            string                     `json:"name"`
	FabricID        string                     `json:"fabric_id"`
	TemplateID      string                     `json:"template_id"`
	TemplateVersion string                     `json:"template_version"`
	Parameters      map[string]json.RawMessage `json:"parameters"`
}

// Creation is durable intent, not proof that a provider allocated a resource.
// A newly created Runner has no machine binding or execution capability.
type Creation struct {
	Runner    runner.Runner
	Operation Operation
	Spec      CreateSpec
}

func boundedText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

// Encode freezes public input, sorts parameter keys and removes JSON whitespace.
// Numeric spellings are preserved so no conversion through float64 changes a
// requested integer or a future provider's interpretation of a decimal value.
func (s CreateSpec) Encode() (string, error) {
	s.Name = strings.TrimSpace(s.Name)
	if !boundedText(s.Name, 120) || !boundedText(s.FabricID, 128) || s.FabricID == "attached" || !boundedText(s.TemplateID, 128) || !boundedText(s.TemplateVersion, 128) || len(s.Parameters) > 32 {
		return "", fmt.Errorf("invalid managed creation specification")
	}
	parameters := make(map[string]json.RawMessage, len(s.Parameters))
	total := 0
	for key, raw := range s.Parameters {
		total += len(key) + len(raw)
		if !boundedText(key, 64) || len(raw) > 4096 || total > 16*1024 || !utf8.Valid(raw) || !json.Valid(raw) {
			return "", fmt.Errorf("invalid managed template parameters")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var scalar any
		if err := decoder.Decode(&scalar); err != nil {
			return "", fmt.Errorf("invalid managed template parameter")
		}
		switch scalar.(type) {
		case string, bool, json.Number:
		default:
			return "", fmt.Errorf("managed template parameters must be scalar values")
		}
		value, err := json.Marshal(scalar)
		if err != nil {
			return "", err
		}
		parameters[key] = value
	}
	s.Parameters = parameters
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	if len(encoded) > 16*1024 {
		return "", fmt.Errorf("managed creation specification exceeds 16 KiB")
	}
	return string(encoded), nil
}
