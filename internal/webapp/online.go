package webapp

import (
	"context"
	"time"

	"github.com/aiomni/dune/internal/authorization"
)

// Call only after resource authorization. The cluster lookup receives this
// page's IDs, not a discovery predicate, and is bounded independently of SQL.
func (s *Server) online(ctx context.Context, resources []authorization.Resource) (map[string]bool, error) {
	ids := make([]string, 0, len(resources))
	for _, resource := range resources {
		if b := resource.Runner.Binding; b != nil {
			ids = append(ids, b.MachineID)
		}
	}
	if s.options.Online != nil {
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return s.options.Online(bounded, ids)
	}
	online := make(map[string]bool, len(ids))
	for _, id := range ids {
		online[id] = s.gateway.Online(id)
	}
	return online, nil
}
