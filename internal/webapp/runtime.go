package webapp

import (
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/aiomni/dune/pkg/api"
)

// A browser subscription retains the runtime selected by the user. Resolving
// an ID alone must never upgrade an old request to a new execution identity.
func selectedRuntime(w http.ResponseWriter, r *http.Request) (api.Runtime, bool) {
	query := r.URL.Query()
	generation, err := strconv.ParseUint(query.Get("generation"), 10, 64)
	selected := api.Runtime{ID: r.PathValue("runtime"), Incarnation: query.Get("incarnation"), Generation: generation}
	if err != nil || generation == 0 || selected.ID == "" || len(selected.ID) > 256 || selected.Incarnation == "" || len(selected.Incarnation) > 256 || strings.ContainsFunc(selected.Incarnation, unicode.IsControl) || len(query["generation"]) != 1 || len(query["incarnation"]) != 1 {
		writeError(w, 400, "INVALID_RUNTIME", "a complete selected Runtime identity is required")
		return selected, false
	}
	return selected, true
}

func sameRuntime(a, b api.Runtime) bool {
	return a.ID == b.ID && a.Incarnation == b.Incarnation && a.Generation == b.Generation
}
