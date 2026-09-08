package fabric

import (
	"encoding/json"
	"strings"
	"testing"
)

func creationSpec() CreateRequest {
	return CreateRequest{Name: " Development ", FabricID: "sandbox", TemplateID: "small", TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"cpu": json.RawMessage(` 9007199254740993 `), "label": json.RawMessage(`"work"`)}}
}

func TestCreateRequestFreezesExactPublicInput(t *testing.T) {
	spec := creationSpec()
	encoded, err := spec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var frozen CreateRequest
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

func TestCreateRequestRejectsUnboundedAndStructuredInput(t *testing.T) {
	cases := map[string]func(*CreateRequest){
		"blank name":      func(s *CreateRequest) { s.Name = "  " },
		"control name":    func(s *CreateRequest) { s.Name = "bad\nname" },
		"invalid UTF8":    func(s *CreateRequest) { s.TemplateID = string([]byte{0xff}) },
		"missing fabric":  func(s *CreateRequest) { s.FabricID = "" },
		"padded fabric":   func(s *CreateRequest) { s.FabricID = " sandbox" },
		"attached fabric": func(s *CreateRequest) { s.FabricID = "attached" },
		"missing version": func(s *CreateRequest) { s.TemplateVersion = "" },
		"long name":       func(s *CreateRequest) { s.Name = strings.Repeat("x", 121) },
		"long field":      func(s *CreateRequest) { s.Parameters[strings.Repeat("x", 65)] = json.RawMessage(`true`) },
		"many fields": func(s *CreateRequest) {
			for i := 0; i < 33; i++ {
				s.Parameters[string(rune('a'+i))] = json.RawMessage(`true`)
			}
		},
		"long value": func(s *CreateRequest) { s.Parameters["x"] = json.RawMessage(`"` + strings.Repeat("x", 4095) + `"`) },
		"large request": func(s *CreateRequest) {
			for _, key := range []string{"a", "b", "c", "d", "e"} {
				s.Parameters[key] = json.RawMessage(`"` + strings.Repeat("x", 4000) + `"`)
			}
		},
	}
	for _, value := range []string{`null`, `{}`, `[]`, `true false`, `"\xff"`, `"\ud800"`, `"\udc00"`, `"\ud800\u0061"`, `NaN`, string([]byte{'"', 0xff, '"'})} {
		cases["value "+value] = func(s *CreateRequest) { s.Parameters["x"] = json.RawMessage(value) }
	}
	validPair := creationSpec()
	validPair.Parameters["emoji"] = json.RawMessage(`"\ud83d\ude80"`)
	if _, err := validPair.Encode(); err != nil {
		t.Fatal("valid surrogate pair rejected", err)
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
