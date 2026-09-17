package daemon

import (
	"context"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/service"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/transport/ws"
	"log"
	"time"
)

func Run(ctx context.Context, c config.Config, readyFile, readyNonce string) error {
	if e := c.Validate(); e != nil {
		return e
	}
	d, e := fabricd.Open(ctx, c.SessionDir)
	if e != nil {
		return e
	}
	defer d.Close()
	tc, e := c.TLS()
	if e != nil {
		return e
	}
	if e := service.WriteStartupReceipt(readyFile, readyNonce); e != nil {
		return e
	}
	lastFailure := ""
	delay := 100 * time.Millisecond
	for ctx.Err() == nil {
		conn, e := ws.Dial(ctx, c.Gateway, c.Token, tc)
		if e != nil && e.Error() != lastFailure {
			lastFailure = e.Error()
			log.Printf("Gateway connection unavailable: %v", e)
		}
		if e == nil {
			lastFailure = ""
			if d.ServeConn(ctx, conn, c.Target) == nil {
				delay = 100 * time.Millisecond
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
	return nil
}
