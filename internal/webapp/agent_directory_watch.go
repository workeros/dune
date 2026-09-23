package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/pkg/agents"
)

func (s *Server) watchAgentDirectory(w http.ResponseWriter, r *http.Request) {
	user, owner, ok := s.workbenchOwner(w, r, "workspace.read")
	if !ok {
		return
	}
	_, token, ok := s.user(w, r)
	if !ok {
		return
	}
	if s.options.AgentDirectoryObserver == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "directory subscriptions are not configured")
		return
	}
	subscription, err := s.options.AgentDirectoryObserver.Subscribe(r.Context(), agents.Scope{Principal: user, OwnerID: owner}, agents.DirectoryWatch{RunnerIDs: r.URL.Query()["runner_id"]})
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	defer subscription.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func(event agents.DirectoryEvent) error {
		// Reauthenticate immediately before each write, including heartbeats.
		// The Go host service validates Runner access; this boundary also owns
		// browser credential revocation and Tenant workspace authorization.
		checked, err := s.identity.Authenticate(r.Context(), token)
		if err != nil || !checked.Valid() || checked.User.ID != user.ID || checked.User.Namespace != user.Namespace || r.Context().Err() != nil {
			return fmt.Errorf("directory identity is no longer valid")
		}
		if _, err := s.access.Check(r.Context(), checked.User, authorization.Resource{OwnerID: owner}, "workspace.read", ""); err != nil {
			return err
		}
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		return controller.Flush()
	}
	if send(agents.DirectoryEvent{SubscriptionID: subscription.ID(), Kind: agents.DirectoryReady}) != nil {
		return
	}
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		event, err := subscription.Next(ctx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil {
			if send(agents.DirectoryEvent{SubscriptionID: subscription.ID(), Kind: "heartbeat"}) != nil {
				return
			}
			continue
		}
		if err != nil {
			_ = send(agents.DirectoryEvent{SubscriptionID: subscription.ID(), Kind: agents.DirectoryInvalidated, Code: "RESYNC_REQUIRED"})
			return
		}
		if send(event) != nil {
			return
		}
	}
}
