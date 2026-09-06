package gateway

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/transport/tunnel"
	"github.com/valyala/fasthttp"
	"log"
	"net"
	"strings"
	"time"
)

func Run(ctx context.Context, c config.Config) error {
	if e := c.ValidateServer(); e != nil {
		return e
	}
	ln, e := net.Listen("tcp", c.Listen)
	if e != nil {
		return e
	}
	if strings.HasPrefix(c.Gateway, "wss://") {
		cert, e := tls.LoadX509KeyPair(c.Certificate, c.Key)
		if e != nil {
			ln.Close()
			return e
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	g := gateway.New()
	defer g.Close()
	handler := tunnel.NewHandler(ctx, g, func(token string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		if subtle.ConstantTimeCompare([]byte(token), []byte(c.Token)) != 1 {
			return gateway.BindingContext{}, nil, fmt.Errorf("unauthorized")
		}
		return (access.Grant{Target: c.Target, Role: gateway.RoleEither}).Bind()
	})
	srv := &fasthttp.Server{Handler: func(c *fasthttp.RequestCtx) {
		if string(c.Path()) != "/tunnel" {
			c.Error("not found", 404)
			return
		}
		handler.ServeFastHTTP(c)
	}, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxRequestBodySize: 1024, Concurrency: 64}
	go func() {
		<-ctx.Done()
		ln.Close()
		g.Close()
	}()
	log.Printf("gateway listening %s", c.Listen)
	e = srv.Serve(ln)
	if ctx.Err() != nil {
		return nil
	}
	return e
}
