package webapp

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	remote = remote.Unmap()
	trusted := func(ip netip.Addr) bool {
		for _, prefix := range s.trustedProxies {
			if prefix.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(remote) {
		return remote.String()
	}
	raw := r.Header.Get("X-Forwarded-For")
	if raw == "" || len(raw) > 8192 {
		return remote.String()
	}
	hops := strings.Split(raw, ",")
	if len(hops) > 32 {
		return remote.String()
	}
	addresses := make([]netip.Addr, len(hops))
	for i, hop := range hops {
		ip, err := netip.ParseAddr(strings.TrimSpace(hop))
		if err != nil {
			return remote.String()
		}
		addresses[i] = ip.Unmap()
	}
	source := remote
	for i := len(addresses) - 1; i >= 0 && trusted(source); i-- {
		source = addresses[i]
	}
	return source.String()
}
