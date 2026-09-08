package fabric

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrTemplateNotFound = errors.New("managed template not found")
var ErrInvalidTemplate = errors.New("invalid managed template configuration")
var ErrInvalidParameters = errors.New("invalid managed template parameters")

// Field describes a public user input, never credentials or private SDK options.
// Integer bounds are decimal strings in JSON to preserve int64 precision in
// browsers. Strings are limited in UTF-8 bytes; choices are exact string values.
type Field struct {
	Name      string   `json:"name"`
	Label     string   `json:"label"`
	Type      string   `json:"type"` // string, integer, boolean
	Required  bool     `json:"required"`
	Minimum   *int64   `json:"minimum,omitempty,string"`
	Maximum   *int64   `json:"maximum,omitempty,string"`
	MaxLength int      `json:"max_length,omitempty"`
	Choices   []string `json:"choices,omitempty"`
}

// Template contains only public configuration. Private provider settings and
// secrets belong in the host's adapter. Version changes whenever its semantics
// or defaults change. Disabled versions may remain configured for recovery.
type Template struct {
	FabricID string  `json:"fabric_id"`
	ID       string  `json:"id"`
	Version  string  `json:"version"`
	Name     string  `json:"name"`
	Disabled bool    `json:"disabled"`
	Fields   []Field `json:"fields"`
}

// TemplateStatus combines immutable public configuration with the provider's
// current willingness to accept new allocations. Unavailable templates remain
// visible to authorized users so the workbench can explain why creation is
// disabled; static Disabled template versions are still omitted entirely.
type TemplateStatus struct {
	Template
	Availability
}

type templateKey struct{ fabric, id, version string }

// Catalog is an immutable, bounded configuration snapshot. It performs no
// authorization, persistence or provider I/O. A service checks discovery/access
// before returning templates or accepting creation requests.
type Catalog struct {
	templates   []Template
	index       map[templateKey]int
	fingerprint string
}

func cloneTemplate(t Template) Template {
	t.Fields = append([]Field(nil), t.Fields...)
	for i := range t.Fields {
		f := &t.Fields[i]
		if f.Minimum != nil {
			v := *f.Minimum
			f.Minimum = &v
		}
		if f.Maximum != nil {
			v := *f.Maximum
			f.Maximum = &v
		}
		f.Choices = append([]string(nil), f.Choices...)
	}
	return t
}

func validateTemplate(t Template) error {
	if !boundedIdentifier(t.FabricID, 128) || t.FabricID == "attached" || !boundedIdentifier(t.ID, 128) || !boundedIdentifier(t.Version, 128) || !boundedText(t.Name, 120) || t.Name != strings.TrimSpace(t.Name) || len(t.Fields) > 32 {
		return ErrInvalidTemplate
	}
	names := make(map[string]bool, len(t.Fields))
	for _, f := range t.Fields {
		if !boundedIdentifier(f.Name, 64) || !boundedText(f.Label, 120) || f.Label != strings.TrimSpace(f.Label) || names[f.Name] {
			return ErrInvalidTemplate
		}
		names[f.Name] = true
		switch f.Type {
		case "string":
			if f.Minimum != nil || f.Maximum != nil || f.MaxLength < 1 || f.MaxLength > 1024 || len(f.Choices) > 64 {
				return ErrInvalidTemplate
			}
			seen := make(map[string]bool, len(f.Choices))
			for _, v := range f.Choices {
				if !boundedText(v, f.MaxLength) || seen[v] {
					return ErrInvalidTemplate
				}
				seen[v] = true
			}
		case "integer":
			if f.Minimum == nil || f.Maximum == nil || *f.Minimum > *f.Maximum || f.MaxLength != 0 || len(f.Choices) != 0 {
				return ErrInvalidTemplate
			}
		case "boolean":
			if f.Minimum != nil || f.Maximum != nil || f.MaxLength != 0 || len(f.Choices) != 0 {
				return ErrInvalidTemplate
			}
		default:
			return ErrInvalidTemplate
		}
	}
	return nil
}

// NewCatalog accepts at most 64 configured versions and 128 KiB of public
// configuration, with at most 16 KiB per template. These bounds allow discovery
// to inspect the whole catalog without exposing unauthorized counts or cursors.
func NewCatalog(templates []Template) (*Catalog, error) {
	if len(templates) > 64 {
		return nil, ErrInvalidTemplate
	}
	catalog := &Catalog{index: make(map[templateKey]int), templates: make([]Template, 0, len(templates))}
	total := 0
	for _, t := range templates {
		if err := validateTemplate(t); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(t)
		if err != nil {
			return nil, ErrInvalidTemplate
		}
		total += len(encoded)
		if len(encoded) > 16*1024 || total > 128*1024 {
			return nil, ErrInvalidTemplate
		}
		catalog.templates = append(catalog.templates, cloneTemplate(t))
	}
	sort.Slice(catalog.templates, func(i, j int) bool {
		a, b := catalog.templates[i], catalog.templates[j]
		if a.FabricID != b.FabricID {
			return a.FabricID < b.FabricID
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Version < b.Version
	})
	for i, t := range catalog.templates {
		key := templateKey{t.FabricID, t.ID, t.Version}
		if _, ok := catalog.index[key]; ok {
			return nil, ErrInvalidTemplate
		}
		catalog.index[key] = i
	}
	encoded, err := json.Marshal(catalog.templates)
	if err != nil {
		return nil, ErrInvalidTemplate
	}
	catalog.fingerprint = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return catalog, nil
}

// Fingerprint covers public templates, versions and availability. Hosts can use
// it in their configuration admission identity; it does not include secrets.
func (c *Catalog) Fingerprint() string { return c.fingerprint }
func (c *Catalog) Template(fabric, id, version string) (Template, error) {
	i, ok := c.index[templateKey{fabric, id, version}]
	if !ok {
		return Template{}, ErrTemplateNotFound
	}
	return cloneTemplate(c.templates[i]), nil
}
func (c *Catalog) Available() []Template {
	out := []Template{}
	for _, t := range c.templates {
		if !t.Disabled {
			out = append(out, cloneTemplate(t))
		}
	}
	return out
}

// ValidateRequest rejects unknown/disabled versions, undeclared fields, absent
// required fields, type coercion and out-of-range input. It returns a canonical
// independent copy suitable for persistence and the exact template version.
func (c *Catalog) ValidateRequest(request CreateRequest) (CreateRequest, error) {
	t, err := c.Template(request.FabricID, request.TemplateID, request.TemplateVersion)
	if err != nil || t.Disabled {
		return CreateRequest{}, ErrTemplateNotFound
	}
	encoded, err := request.Encode()
	if err != nil {
		return CreateRequest{}, ErrInvalidParameters
	}
	var frozen CreateRequest
	if err := json.Unmarshal([]byte(encoded), &frozen); err != nil {
		return CreateRequest{}, ErrInvalidParameters
	}
	fields := make(map[string]Field, len(t.Fields))
	for _, f := range t.Fields {
		fields[f.Name] = f
		if _, ok := frozen.Parameters[f.Name]; f.Required && !ok {
			return CreateRequest{}, ErrInvalidParameters
		}
	}
	for name, raw := range frozen.Parameters {
		f, ok := fields[name]
		if !ok {
			return CreateRequest{}, ErrInvalidParameters
		}
		switch f.Type {
		case "string":
			var value string
			if json.Unmarshal(raw, &value) != nil || (value != "" && !boundedText(value, f.MaxLength)) || (f.Required && strings.TrimSpace(value) == "") {
				return CreateRequest{}, ErrInvalidParameters
			}
			if len(f.Choices) > 0 {
				found := false
				for _, choice := range f.Choices {
					found = found || value == choice
				}
				if !found {
					return CreateRequest{}, ErrInvalidParameters
				}
			}
		case "integer":
			// Int64 rejects fractional/exponential spellings and quoted strings. No
			// float64 conversion can admit an integer just beyond a declared boundary.
			if len(raw) == 0 || raw[0] == '"' {
				return CreateRequest{}, ErrInvalidParameters
			}
			value, err := json.Number(string(raw)).Int64()
			if err != nil || value < *f.Minimum || value > *f.Maximum {
				return CreateRequest{}, ErrInvalidParameters
			}
		case "boolean":
			if string(raw) != "true" && string(raw) != "false" {
				return CreateRequest{}, ErrInvalidParameters
			}
		}
	}
	return frozen, nil
}
