package agentservice

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/workbench"
)

type operationReference struct {
	Target workbench.AgentTarget `json:"target"`
	ID     string                `json:"id"`
}

func operationRef(target workbench.AgentTarget, id string) string {
	encoded, _ := json.Marshal(operationReference{Target: target, ID: id})
	return "operation_" + base64.RawURLEncoding.EncodeToString(encoded)
}

func parseOperationRef(value string) (operationReference, error) {
	var ref operationReference
	if len(value) > 8*1024 || !strings.HasPrefix(value, "operation_") {
		return ref, invalid("invalid operation reference")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "operation_"))
	if err != nil {
		return ref, invalid("invalid operation reference")
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ref) != nil || ref.Target.Validate() != nil || !wire.ValidID(ref.ID) || operationRef(ref.Target, ref.ID) != value {
		return operationReference{}, invalid("invalid operation reference")
	}
	return ref, nil
}
