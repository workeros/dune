package webapp

import (
	"net/http"

	"github.com/aiomni/dune/pkg/identity"
)

// Startup information is an anonymous, finite description of the assembled
// application. It never includes credentials or provider configuration.
type startupInfo struct {
	LoginMethods      []identity.LoginMethod `json:"login_methods"`
	LocalRegistration bool                   `json:"local_registration"`
	Attached          bool                   `json:"attached"`
	Managed           bool                   `json:"managed"`
	TenantScoped      bool                   `json:"tenant_scoped"`
	PublicURL         string                 `json:"public_url"`
	GatewayURL        string                 `json:"gateway_url"`
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	methods := s.identity.LoginMethods(s.urls.PublicURL)
	password, local := s.identity.(identity.PasswordService)
	registration := local && password.RegistrationAllowed()
	writeJSON(w, http.StatusOK, startupInfo{
		LoginMethods:      methods,
		LocalRegistration: registration,
		Attached:          !s.options.DisableAttached,
		Managed:           s.options.Managed != nil,
		TenantScoped:      s.options.TenantScoped,
		PublicURL:         s.urls.PublicURL,
		GatewayURL:        s.urls.GatewayURL,
	})
}
