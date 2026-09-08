package lifecycle

import (
	"encoding/json"
	"strings"
	"testing"
)

func creationSpec() CreateSpec {
	return CreateSpec{Name: " Development ", FabricID: "sandbox", TemplateID: "small", TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"cpu": json.RawMessage(` 9007199254740993 `), "label": json.RawMessage(`"work"`)}}
}

func TestCreateSpecFreezesExactPublicInput(t *testing.T) {
	spec := creationSpec()
	encoded, err := spec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var frozen CreateSpec
	if err := json.Unmarshal([]byte(encoded), &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.Name != "Development" || string(frozen.Parameters["cpu"]) != "9007199254740993" {
		t.Fatal("normalization changed numeric precision or name", encoded)
	}
	again, err := frozen.Encode()
	if err != nil || again != encoded {
		t.Fatal("snapshot is not canonical", err)
	}
	spec.Parameters["cpu"][1] = '1'
	delete(spec.Parameters, "label")
	if string(frozen.Parameters["cpu"]) != "9007199254740993" || len(frozen.Parameters) != 2 {
		t.Fatal("frozen input aliases caller memory")
	}
	nilParameters := creationSpec()
	nilParameters.Parameters = nil
	emptyParameters := nilParameters
	emptyParameters.Parameters = map[string]json.RawMessage{}
	nilJSON, _ := nilParameters.Encode()
	emptyJSON, _ := emptyParameters.Encode()
	if nilJSON != emptyJSON {
		t.Fatal("equivalent empty parameters have different digests")
	}
	// Decimal spellings stay distinct: no float64 round-trip or coercion decides
	// the semantics of a template's public numeric fields.
	for _, number := range []string{"1", "1.0", "1e0", "-0", "1e9999"} {
		s := creationSpec()
		s.Parameters = map[string]json.RawMessage{"n": json.RawMessage(number)}
		encoded, err := s.Encode()
		if err != nil || !strings.Contains(encoded, `"n":`+number) {
			t.Fatal("numeric spelling changed", number, err)
		}
	}
}

func TestCreateSpecRejectsUnboundedAndStructuredInput(t *testing.T) {
	cases := map[string]func(*CreateSpec){
		"blank name":      func(s *CreateSpec) { s.Name = "  " },
		"control name":    func(s *CreateSpec) { s.Name = "bad\nname" },
		"invalid UTF8":    func(s *CreateSpec) { s.TemplateID = string([]byte{0xff}) },
		"missing fabric":  func(s *CreateSpec) { s.FabricID = "" },
		"attached fabric": func(s *CreateSpec) { s.FabricID = "attached" },
		"missing version": func(s *CreateSpec) { s.TemplateVersion = "" },
		"long name":       func(s *CreateSpec) { s.Name = strings.Repeat("x", 121) },
		"long field":      func(s *CreateSpec) { s.Parameters[strings.Repeat("x", 65)] = json.RawMessage(`true`) },
		"many fields": func(s *CreateSpec) {
			for i := 0; i < 33; i++ {
				s.Parameters[string(rune('a'+i))] = json.RawMessage(`true`)
			}
		},
		"long value": func(s *CreateSpec) { s.Parameters["x"] = json.RawMessage(`"` + strings.Repeat("x", 4095) + `"`) },
		"large request": func(s *CreateSpec) {
			for _, key := range []string{"a", "b", "c", "d", "e"} {
				s.Parameters[key] = json.RawMessage(`"` + strings.Repeat("x", 4000) + `"`)
			}
		},
	}
	for _, value := range []string{`null`, `{}`, `[]`, `true false`, `"\xff"`, `NaN`, string([]byte{'"', 0xff, '"'})} {
		cases["value "+value] = func(s *CreateSpec) { s.Parameters["x"] = json.RawMessage(value) }
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			spec := creationSpec()
			change(&spec)
			if _, err := spec.Encode(); err == nil {
				t.Fatal("invalid specification accepted")
			}
		})
	}
}
