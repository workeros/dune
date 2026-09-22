package fabricd

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/google/jsonschema-go/jsonschema"
)

var credentialField = regexp.MustCompile(`(?i)password|passphrase|api.?key|access.?token|refresh.?token|private.?key|recovery.?code|credit.?card|card.?number|cvv|密码|密钥|令牌|验证码|恢复码|银行卡`)

// ACP forms are a small, flat subset of JSON Schema. Normalize nullable
// annotations and constraints before handing validation to the schema library.
// Unrecognized shapes remain inspectable/declinable, never guessed as inputs.
func prepareElicitationSchema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return nil, fmt.Errorf("form schema must be an object")
	}
	if root["type"] == nil {
		root["type"] = "object"
	}
	if root["type"] != "object" {
		return nil, fmt.Errorf("form schema must describe an object")
	}
	if err := restrictSchemaKeys(root, "type properties required title description"); err != nil {
		return nil, err
	}
	properties := map[string]any{}
	if value, ok := root["properties"]; ok {
		var valid bool
		properties, valid = value.(map[string]any)
		if !valid {
			return nil, fmt.Errorf("invalid form properties")
		}
	}
	if len(properties) > 64 {
		return nil, fmt.Errorf("form has more than 64 fields")
	}
	for name, value := range properties {
		field, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid field schema")
		}
		label, _ := field["title"].(string)
		if credentialField.MatchString(name + " " + label) {
			return nil, fmt.Errorf("credentials require URL mode; this form cannot be submitted")
		}
		if err := prepareElicitationField(field); err != nil {
			return nil, err
		}
	}
	root["properties"] = properties
	root["additionalProperties"] = false
	var schema jsonschema.Schema
	if err := json.Unmarshal(api.Payload(root), &schema); err != nil {
		return nil, fmt.Errorf("unsupported form schema")
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{})
	if err != nil {
		return nil, fmt.Errorf("invalid or unsupported form constraint")
	}
	return resolved, nil
}

func restrictSchemaKeys(schema map[string]any, keys string) error {
	allowed := " " + keys + " "
	for key, value := range schema {
		if key == "_meta" || value == nil {
			delete(schema, key)
			continue
		}
		if !strings.Contains(allowed, " "+key+" ") {
			return fmt.Errorf("unsupported form field or constraint: %s", key)
		}
	}
	return nil
}

func prepareElicitationField(field map[string]any) error {
	keys := "type title description default"
	switch field["type"] {
	case "string":
		keys += " enum oneOf format minLength maxLength pattern"
		if format, ok := field["format"].(string); ok && format != "email" && format != "uri" && format != "date" && format != "date-time" {
			return fmt.Errorf("unsupported string format")
		}
		if values, exists := field["enum"]; exists && values != nil {
			if err := validateStringEnum(values); err != nil {
				return err
			}
		}
		if options, exists := field["oneOf"]; exists && options != nil {
			if err := validateEnumOptions(options); err != nil {
				return err
			}
		}
	case "number", "integer":
		keys += " minimum maximum"
	case "boolean":
	case "array":
		keys += " items minItems maxItems"
		items, ok := field["items"].(map[string]any)
		if !ok {
			return fmt.Errorf("multi-select requires enum items")
		}
		if items["type"] == "string" {
			if err := restrictSchemaKeys(items, "type enum"); err != nil {
				return err
			}
			if err := validateStringEnum(items["enum"]); err != nil {
				return err
			}
		} else if items["type"] == nil && items["anyOf"] != nil {
			if err := restrictSchemaKeys(items, "anyOf"); err != nil {
				return err
			}
			if err := validateEnumOptions(items["anyOf"]); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unsupported multi-select item type")
		}
	default:
		return fmt.Errorf("unsupported form field type")
	}
	return restrictSchemaKeys(field, keys)
}

func validateEnumOptions(value any) error {
	options, ok := value.([]any)
	if !ok || len(options) == 0 {
		return fmt.Errorf("invalid enum options")
	}
	for _, value := range options {
		option, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid enum option")
		}
		if err := restrictSchemaKeys(option, "const title description"); err != nil {
			return err
		}
		if _, ok := option["const"].(string); !ok {
			return fmt.Errorf("enum values must be strings")
		}
	}
	return nil
}

// jsonschema-go treats format as an annotation. ACP's four string formats
// still need explicit checks; returned errors never include submitted values.
func validateElicitationFormats(raw json.RawMessage, values map[string]any) error {
	var schema struct {
		Properties map[string]struct {
			Format string `json:"format"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(raw, &schema)
	for name, field := range schema.Properties {
		value, ok := values[name].(string)
		if !ok || field.Format == "" {
			continue
		}
		valid := false
		switch field.Format {
		case "email":
			address, err := mail.ParseAddress(value)
			valid = err == nil && address.Address == value
		case "uri":
			parsed, err := url.Parse(value)
			valid = err == nil && parsed.IsAbs()
		case "date":
			_, err := time.Parse("2006-01-02", value)
			valid = err == nil
		case "date-time":
			_, err := time.Parse(time.RFC3339, value)
			valid = err == nil
		}
		if !valid {
			return fmt.Errorf("form string format does not match schema")
		}
	}
	return nil
}

func validateStringEnum(value any) error {
	values, ok := value.([]any)
	if !ok || len(values) == 0 {
		return fmt.Errorf("enum requires string values")
	}
	for _, value := range values {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("enum values must be strings")
		}
	}
	return nil
}
