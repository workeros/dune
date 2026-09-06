package webapp

import "net/http"

// Startup information is an anonymous, finite description of the assembled
// application. It never includes credentials or provider configuration.
type startupInfo struct {
	LoginMethods      []loginMethod `json:"login_methods"`
	LocalRegistration bool          `json:"local_registration"`
	Attached          bool          `json:"attached"`
	Managed           bool          `json:"managed"`
	PublicURL         string        `json:"public_url"`
	GatewayURL        string        `json:"gateway_url"`
}

type loginMethod struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	methods := []loginMethod{{Kind: "password", URL: s.urls.PublicURL + "api/auth/login"}}
	if s.options.External != nil {
		methods = []loginMethod{{Kind: "external", URL: s.urls.PublicURL + "api/auth/external/start"}}
	}
	writeJSON(w, http.StatusOK, startupInfo{
		LoginMethods:      methods,
		LocalRegistration: s.identity.RegistrationAllowed(),
		Attached:          true,
		Managed:           false,
		PublicURL:         s.urls.PublicURL,
		GatewayURL:        s.urls.GatewayURL,
	})
}
