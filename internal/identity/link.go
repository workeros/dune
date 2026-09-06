package identity

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	public "github.com/aiomni/dune/pkg/identity"
)

func ValidateLink(r public.LinkRequest) error {
	for _, field := range []struct {
		value string
		limit int
	}{{r.RequestID, 128}, {r.Actor, 256}, {r.PrincipalID, 128}, {r.Namespace, 2048}, {r.Subject, 512}, {r.Reason, 1024}} {
		if strings.TrimSpace(field.value) == "" || len(field.value) > field.limit || !utf8.ValidString(field.value) || strings.ContainsFunc(field.value, unicode.IsControl) {
			return fmt.Errorf("identity link requires bounded, nonempty identifiers, actor and reason without control characters")
		}
	}
	return nil
}
