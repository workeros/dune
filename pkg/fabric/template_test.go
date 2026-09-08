package fabric

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func intBound(v int64) *int64 { return &v }
func templateFixture() Template {
	return Template{FabricID: "sandbox", ID: "small", Version: "v1", Name: "Small environment", Fields: []Field{
		{Name: "cpu", Label: "CPU cores", Type: "integer", Required: true, Minimum: intBound(1), Maximum: intBound(8)},
		{Name: "region", Label: "Region", Type: "string", Required: true, MaxLength: 32, Choices: []string{"east", "west"}},
		{Name: "label", Label: "Label", Type: "string", MaxLength: 12},
		{Name: "network", Label: "Network", Type: "boolean"},
	}}
}
func templateRequest() CreateRequest {
	return CreateRequest{Name: "Development", FabricID: "sandbox", TemplateID: "small", TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"cpu": json.RawMessage(`2`), "region": json.RawMessage(`"east"`)}}
}

func TestTemplateCatalogIsImmutableAndVersioned(t *testing.T) {
	source := templateFixture()
	catalog, err := NewCatalog([]Template{source})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := catalog.Fingerprint()
	*source.Fields[0].Maximum = 1
	source.Fields[1].Choices[0] = "replacement"
	got, err := catalog.Template("sandbox", "small", "v1")
	if err != nil || *got.Fields[0].Maximum != 8 || got.Fields[1].Choices[0] != "east" {
		t.Fatal("catalog retained mutable configuration", err)
	}
	*got.Fields[0].Maximum = 0
	got.Fields[1].Choices[0] = "changed"
	available := catalog.Available()
	available[0].Fields[1].Choices[0] = "changed again"
	if _, err := catalog.ValidateRequest(templateRequest()); err != nil {
		t.Fatal("caller rewrote validation rules", err)
	}
	if catalog.Fingerprint() != fingerprint {
		t.Fatal("caller changed configuration identity")
	}
	second := templateFixture()
	second.ID = "large"
	a, _ := NewCatalog([]Template{templateFixture(), second})
	b, _ := NewCatalog([]Template{second, templateFixture()})
	if a.Fingerprint() != b.Fingerprint() || !reflect.DeepEqual(a.Available(), b.Available()) {
		t.Fatal("configuration depends on input order")
	}
	disabled := templateFixture()
	disabled.Disabled = true
	c, err := NewCatalog([]Template{disabled})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Available()) != 0 || c.Fingerprint() == fingerprint {
		t.Fatal("disabled creation remains advertised")
	}
	if _, err := c.Template("sandbox", "small", "v1"); err != nil {
		t.Fatal("disabled version disappeared from recovery configuration", err)
	}
	if _, err := c.ValidateRequest(templateRequest()); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatal("disabled version accepted new creation", err)
	}
	request := templateRequest()
	request.TemplateVersion = "v2"
	if _, err := catalog.ValidateRequest(request); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatal("unknown version fell back to current template", err)
	}
}

func TestTemplateParametersRequireExactTypesAndRanges(t *testing.T) {
	catalog, err := NewCatalog([]Template{templateFixture()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, key, value string }{
		{"below range", "cpu", `0`}, {"above range", "cpu", `9`}, {"quoted integer", "cpu", `"2"`}, {"fractional", "cpu", `2.0`}, {"exponential", "cpu", `2e0`},
		{"wrong choice", "region", `"private"`}, {"blank required", "region", `""`}, {"wrong string type", "label", `false`}, {"long string", "label", `"longer than twelve"`}, {"control string", "label", `"line\nbreak"`},
		{"boolean coercion", "network", `1`}, {"boolean string", "network", `"false"`}, {"unknown field", "provider_secret", `"not accepted"`}, {"missing field", "cpu", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := templateRequest()
			if tc.value == "" {
				delete(req.Parameters, tc.key)
			} else {
				req.Parameters[tc.key] = json.RawMessage(tc.value)
			}
			if _, err := catalog.ValidateRequest(req); !errors.Is(err, ErrInvalidParameters) {
				t.Fatal("invalid input accepted", err)
			}
		})
	}
	request := templateRequest()
	request.Parameters["network"] = json.RawMessage(`false`)
	request.Parameters["label"] = json.RawMessage(`""`)
	frozen, err := catalog.ValidateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Parameters["cpu"][0] = '8'
	if string(frozen.Parameters["cpu"]) != "2" {
		t.Fatal("accepted request aliases original parameters")
	}
	wide := templateFixture()
	wide.Fields[0].Minimum = intBound(math.MaxInt64 - 1)
	wide.Fields[0].Maximum = intBound(math.MaxInt64)
	exact, err := NewCatalog([]Template{wide})
	if err != nil {
		t.Fatal(err)
	}
	request = templateRequest()
	request.Parameters["cpu"] = json.RawMessage(`9223372036854775807`)
	if _, err := exact.ValidateRequest(request); err != nil {
		t.Fatal("int64 boundary lost precision", err)
	}
	request.Parameters["cpu"] = json.RawMessage(`9223372036854775808`)
	if _, err := exact.ValidateRequest(request); !errors.Is(err, ErrInvalidParameters) {
		t.Fatal("int64 overflow rounded into range", err)
	}
	encoded, err := json.Marshal(wide)
	if err != nil || !strings.Contains(string(encoded), `"maximum":"9223372036854775807"`) {
		t.Fatal("browser metadata lost exact integer bound", err)
	}
}

func TestTemplateConfigurationRejectsAmbiguousAndUnboundedRules(t *testing.T) {
	for name, change := range map[string]func(*Template){
		"duplicate field":      func(t *Template) { t.Fields = append(t.Fields, t.Fields[0]) },
		"unknown type":         func(t *Template) { t.Fields[0].Type = "object" },
		"missing lower bound":  func(t *Template) { t.Fields[0].Minimum = nil },
		"reversed bounds":      func(t *Template) { t.Fields[0].Minimum = intBound(10) },
		"unused constraint":    func(t *Template) { t.Fields[3].MaxLength = 10 },
		"unbounded string":     func(t *Template) { t.Fields[1].MaxLength = 0 },
		"duplicate choice":     func(t *Template) { t.Fields[1].Choices = []string{"east", "east"} },
		"choice exceeds bound": func(t *Template) { t.Fields[1].MaxLength = 2 },
		"missing version":      func(t *Template) { t.Version = "" },
		"padded identifier":    func(t *Template) { t.ID = " small" },
		"padded display name":  func(t *Template) { t.Name = " Small environment" },
		"attached namespace":   func(t *Template) { t.FabricID = "attached" },
		"too many fields": func(t *Template) {
			for i := 0; i < 33; i++ {
				t.Fields = append(t.Fields, Field{Name: strings.Repeat("x", i+1), Label: "x", Type: "boolean"})
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := templateFixture()
			change(&source)
			if _, err := NewCatalog([]Template{source}); !errors.Is(err, ErrInvalidTemplate) {
				t.Fatal("invalid configuration accepted", err)
			}
		})
	}
	source := templateFixture()
	if _, err := NewCatalog([]Template{source, source}); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatal("duplicate version accepted", err)
	}
	if _, err := NewCatalog(make([]Template, 65)); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatal("unbounded catalog accepted", err)
	}
}
