// Package fabric defines public Managed template configuration and requests.
package fabric

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CreateRequest is the immutable, public template selection for one creation.
// The lifecycle service must validate the selected template's field types,
// ranges and allowlist before persistence. Private provider configuration and
// secrets never belong here. Parameters are bounded scalar JSON values; their
// meaning belongs to this exact template version, not the current template.
type CreateRequest struct {
	Name            string                     `json:"name"`
	FabricID        string                     `json:"fabric_id"`
	TemplateID      string                     `json:"template_id"`
	TemplateVersion string                     `json:"template_version"`
	CredentialEmail string                     `json:"credential_email,omitempty"`
	Parameters      map[string]json.RawMessage `json:"parameters"`
}

func boundedText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func boundedIdentifier(value string, limit int) bool {
	return value == strings.TrimSpace(value) && boundedText(value, limit)
}

func hexDigit(value byte) (uint16, bool) {
	switch {
	case value >= '0' && value <= '9':
		return uint16(value - '0'), true
	case value >= 'a' && value <= 'f':
		return uint16(value-'a') + 10, true
	case value >= 'A' && value <= 'F':
		return uint16(value-'A') + 10, true
	default:
		return 0, false
	}
}

// validStringEscapes rejects the unpaired UTF-16 surrogate escapes which
// encoding/json otherwise replaces with U+FFFD. Public parameters must never
// change silently while their immutable snapshot is being encoded.
func validStringEscapes(raw []byte) bool {
	for i := 1; i+1 < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if raw[i+1] != 'u' {
			i++
			continue
		}
		if i+5 >= len(raw) {
			return false
		}
		var value uint16
		for j := i + 2; j <= i+5; j++ {
			digit, ok := hexDigit(raw[j])
			if !ok {
				return false
			}
			value = value*16 + digit
		}
		i += 5
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value < 0xd800 || value > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		var low uint16
		for j := i + 3; j <= i+6; j++ {
			digit, ok := hexDigit(raw[j])
			if !ok {
				return false
			}
			low = low*16 + digit
		}
		if low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

// Encode freezes public input, sorts parameter keys and removes JSON whitespace.
// Numeric spellings are preserved so no conversion through float64 changes a
// requested integer or a future provider's interpretation of a decimal value.
func (s CreateRequest) Encode() (string, error) {
	s.Name = strings.TrimSpace(s.Name)
	if !boundedText(s.Name, 120) || !boundedIdentifier(s.FabricID, 128) || s.FabricID == "attached" || !boundedIdentifier(s.TemplateID, 128) || !boundedIdentifier(s.TemplateVersion, 128) || (s.CredentialEmail != "" && (!boundedText(s.CredentialEmail, 256) || strings.TrimSpace(s.CredentialEmail) != s.CredentialEmail)) || len(s.Parameters) > 32 {
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
		if _, ok := scalar.(string); ok && !validStringEscapes(raw) {
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
