// Package deployment defines the public address contract shared by applications
// and installation clients. Protocol cores do not depend on deployment URLs.
package deployment

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
)

type URLs struct {
	PublicURL  string
	GatewayURL string
	Origin     string
	Path       string
	CookiePath string
}

func parse(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") {
		return nil, fmt.Errorf("absolute URL without user information, query or fragment required")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("empty URL port")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("URL port must be 1..65535")
		}
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsUnspecified() {
		return nil, fmt.Errorf("public URL must not use a wildcard address")
	}
	// Reject ambiguous encodings rather than letting proxies interpret encoded
	// separators or dot segments differently from the application router.
	if u.RawPath != "" || strings.ContainsAny(u.Path, "\\\r\n;\x00") {
		return nil, fmt.Errorf("ambiguous URL path")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, fmt.Errorf("URL path cannot contain dot segments")
		}
	}
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

// Public normalizes the deployment path to end in a slash. Its scheme, host and
// port form the trusted browser Origin; forwarded headers never change it.
func Public(value string) (*url.URL, error) {
	u, err := parse(value)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("public URL must use HTTP or HTTPS")
	}
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		host := u.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		u.Host = host
	}
	u.Path = strings.TrimSuffix(path.Clean("/"+u.Path), "/") + "/"
	return u, nil
}

// Gateway accepts a final WS(S) connection URL, including its full path. It
// never appends /tunnel or infers a browser deployment prefix.
func Gateway(value string) (*url.URL, error) {
	u, err := parse(value)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || u.Path == "" || u.Path == "/" || path.Clean(u.Path) != u.Path {
		return nil, fmt.Errorf("gateway URL must be an absolute WS(S) URL with a complete connection path")
	}
	return u, nil
}

func NewURLs(publicURL, gatewayURL string) (URLs, error) {
	u, err := Public(publicURL)
	if err != nil {
		return URLs{}, err
	}
	if gatewayURL == "" {
		gateway := *u
		gateway.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
		gateway.Path += "tunnel"
		gatewayURL = gateway.String()
	}
	gateway, err := Gateway(gatewayURL)
	if err != nil {
		return URLs{}, err
	}
	// Match the installation client's existing downgrade protection before
	// issuing or consuming one-time registration materials.
	if u.Scheme == "https" && gateway.Scheme != "wss" {
		return URLs{}, fmt.Errorf("HTTPS deployments require a WSS machine URL")
	}
	return URLs{PublicURL: u.String(), GatewayURL: gateway.String(), Origin: u.Scheme + "://" + u.Host, Path: u.Path, CookiePath: u.EscapedPath()}, nil
}
