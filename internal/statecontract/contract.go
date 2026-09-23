// Package statecontract defines the single shared read/write contract supported
// by this baseline. Its digest covers semantics, not merely SQLite column names.
package statecontract

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
)

//go:embed contract.txt
var definition string

// ID is immutable for this executable. A different contract is rejected; there
// is no negotiation, migration, write downgrade or historical-format adapter.
func ID() string { return fmt.Sprintf("%x", sha256.Sum256([]byte(definition))) }
