package api

import (
	"errors"
	"net/url"
	"strings"
)

// AgentMCP is a private launch-time credential, never a Profile or a response.
// The Runner chooses its native transport; callers cannot supply bridge argv.
type AgentMCP struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func (c AgentMCP) Validate() error {
	address, err := url.Parse(c.URL)
	if err != nil || address.Host == "" || (address.Scheme != "http" && address.Scheme != "https") || address.User != nil || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" || len(c.URL) > 4096 || c.Token == "" || len(c.Token) > 1024 || strings.ContainsAny(c.Token, " \r\n\t\x00") {
		return errors.New("invalid Agent MCP endpoint or credential")
	}
	return nil
}

type AgentMCPStatus struct {
	Transport string `json:"transport"`
}
