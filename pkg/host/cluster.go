package host

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/transport/peer"
)

// ClusterOptions selects a shared PostgreSQL directory and a direct, private
// peer endpoint. Replicas must share the recovery generation and trust roots.
// Configure identity and policy consistently across replicas. Switching an
// existing deployment between standalone and cluster mode requires downtime.
type ClusterOptions struct {
	RecoveryGeneration string
	Peer               peer.Config
}

func clusterTransport(database storage.Config, options *ClusterOptions) (*peer.Transport, error) {
	if options == nil {
		return nil, nil
	}
	if database.Postgres == nil || database.SQLiteDir != "" || !wire.ValidID(options.RecoveryGeneration) {
		return nil, fmt.Errorf("cluster requires PostgreSQL and a valid recovery generation")
	}
	return peer.New(options.Peer)
}

func openGateway(ctx context.Context, store *metadata.Store, database storage.Config, options *ClusterOptions, transport *peer.Transport) (*gateway.Gateway, error) {
	if options == nil {
		if database.Postgres != nil {
			_, err := store.ConnectionRecovery(ctx)
			if err == nil {
				return nil, fmt.Errorf("database is initialized for cluster ownership; Cluster options required")
			}
			if !errors.Is(err, metadata.ErrNotFound) {
				return nil, err
			}
		}
		return gateway.New(), nil
	}
	directory, err := store.ConnectionDirectory(ctx, options.RecoveryGeneration)
	if err != nil {
		return nil, err
	}
	return gateway.NewWithPeers(directory, transport.Address(), options.RecoveryGeneration, transport.Dial)
}

func peerAuthorization(service *authorization.Service) peer.Authorize {
	return func(ctx context.Context, source, target string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		if err := ctx.Err(); err != nil {
			return gateway.BindingContext{}, nil, err
		}
		return service.Peer(source, target)
	}
}

// ServePeer takes ownership of a plain TCP listener, including on failure, and
// serves only the private peer endpoint with the configured mutual TLS policy.
// The advertised address must reach this instance directly. Close and parent
// cancellation stop this listener and its upgraded connections along with Web.
func (a *App) ServePeer(listener net.Listener) error {
	if a.peer == nil {
		listener.Close()
		return fmt.Errorf("peer listener requires Cluster options")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.serveHTTP(a.peerHandler, w, r)
	})
	return a.serve(tls.NewListener(listener, a.peer.ServerTLSConfig()), handler)
}
