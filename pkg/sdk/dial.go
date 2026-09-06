package sdk

import (
	"context"
	"crypto/tls"
	"fmt"
	"github.com/aiomni/dune/pkg/transport/ws"
	"net/url"
)

type Options struct {
	Gateway, Token, Target string
	TLSConfig              *tls.Config
}

func Dial(ctx context.Context, o Options) (*Client, error) {
	u, e := url.Parse(o.Gateway)
	if e != nil || u == nil || (u.Scheme != "ws" && u.Scheme != "wss") {
		return nil, fmt.Errorf("Gateway must use ws:// or wss://")
	}
	if u.Scheme == "wss" && (o.TLSConfig == nil || o.TLSConfig.InsecureSkipVerify) {
		return nil, fmt.Errorf("verified TLS configuration required")
	}
	conn, e := ws.Dial(ctx, o.Gateway, o.Token, o.TLSConfig)
	if e != nil {
		return nil, e
	}
	return Connect(ctx, conn, o.Target)
}
