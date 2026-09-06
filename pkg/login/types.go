// Package login provides human authentication above the execution protocol.
// Browser confirmation produces a revocable CLI session; Dial exchanges it for
// a single-use target credential before using the normal execution SDK.
package login

type Request struct {
	ID              string `json:"id"`
	Code            string `json:"code"`
	VerificationURL string `json:"verification_url"`
	ExpiresAt       int64  `json:"expires_at"`
}

type Review struct {
	ID        string `json:"id"`
	Code      string `json:"code"`
	Site      string `json:"site"`
	ExpiresAt int64  `json:"expires_at"`
	Confirmed bool   `json:"confirmed"`
}

// Session is a secret local credential. Do not log or publish Token.
type Session struct {
	Site        string `json:"site" yaml:"site"`
	Token       string `json:"token" yaml:"token"`
	PrincipalID string `json:"principal_id" yaml:"principal_id"`
	ExpiresAt   int64  `json:"expires_at" yaml:"expires_at"`
}

type Access struct {
	Credential string `json:"credential"`
	Gateway    string `json:"gateway"`
	Target     string `json:"target"`
	ExpiresAt  int64  `json:"expires_at"`
}

type Machine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
}

type MachinePage struct {
	Items      []Machine `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}
