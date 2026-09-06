package wire

import (
	"fmt"
	"github.com/fasthttp/websocket"
	"io"
	"net"
	"time"
)

// wsConn erases WebSocket message boundaries. Yamux owns the single reader and
// writer; deadlines go to the socket to avoid racing websocket's write state.
type wsConn struct {
	w *websocket.Conn
	r io.Reader
}

func NetConn(w *websocket.Conn) net.Conn { w.SetReadLimit(MaxMessage); return &wsConn{w: w} }
func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.r == nil {
			k, r, e := c.w.NextReader()
			if e != nil {
				return 0, e
			}
			if k != websocket.BinaryMessage {
				return 0, fmt.Errorf("binary WebSocket messages required")
			}
			c.r = r
		}
		n, e := c.r.Read(p)
		if e == io.EOF {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, e
	}
}
func (c *wsConn) Write(p []byte) (int, error) {
	if e := c.w.WriteMessage(websocket.BinaryMessage, p); e != nil {
		return 0, e
	}
	return len(p), nil
}
func (c *wsConn) Close() error                       { return c.w.Close() }
func (c *wsConn) LocalAddr() net.Addr                { return c.w.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr               { return c.w.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error      { return c.w.UnderlyingConn().SetDeadline(t) }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.w.UnderlyingConn().SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.w.UnderlyingConn().SetWriteDeadline(t) }
