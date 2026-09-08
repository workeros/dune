package lifecycle

import (
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/runner"
)

// Creation is durable intent, not proof that a provider allocated a resource.
// A newly created Runner has no machine binding or execution capability.
type Creation struct {
	Runner    runner.Runner
	Operation Operation
	Spec      fabric.CreateRequest
}
