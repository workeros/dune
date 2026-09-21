package webapp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"github.com/aiomni/dune/internal/metadata"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/pkg/deployment"
)

func EnrollMachine(ctx context.Context, path, site, token, certificate, runnerID string) error {
	u, err := deployment.Public(site)
	if err != nil {
		return err
	}
	site = u.String()
	if len(token) != 64 {
		return fmt.Errorf("one-time binding token required")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("configuration already exists; use repair or upgrade with the existing binding")
	} else if !os.IsNotExist(err) {
		return err
	}
	if runnerID == "" {
		return fmt.Errorf("stable pending Runner ID required")
	}
	unknown := func(cause error) error {
		return fmt.Errorf("enrollment result unknown for Runner %s; if this configuration is complete use repair; otherwise inspect and revoke this exact Runner before generating a new command: %w", runnerID, cause)
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if certificate != "" {
		certificate, err = filepath.Abs(certificate)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(certificate)
		if err != nil {
			return err
		}
		tc.RootCAs = x509.NewCertPool()
		if !tc.RootCAs.AppendCertsFromPEM(b) {
			return fmt.Errorf("invalid trust certificate")
		}
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	body, _ := json.Marshal(map[string]string{"token": token, "os": runtime.GOOS, "arch": runtime.GOARCH})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(site, "/")+"/api/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dune-Request", "1")
	response, err := client.Do(request)
	if err != nil {
		return unknown(err)
	}
	defer response.Body.Close()
	var result struct {
		Machine    metadata.Machine `json:"machine"`
		Credential string           `json:"credential"`
		Gateway    string           `json:"gateway"`
		Error      string           `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 256*1024)).Decode(&result); err != nil {
		return unknown(fmt.Errorf("invalid binding response: %w", err))
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("binding failed (%d): %s", response.StatusCode, result.Error)
	}
	if result.Machine.RunnerID != runnerID {
		return unknown(fmt.Errorf("response does not match the requested Runner"))
	}
	c := config.Config{Gateway: result.Gateway, Token: result.Credential, Target: result.Machine.ID, Certificate: certificate, SessionDir: filepath.Join(filepath.Dir(path), "sessions")}
	if u.Scheme == "https" && !strings.HasPrefix(c.Gateway, "wss://") {
		return unknown(fmt.Errorf("server attempted to downgrade the machine connection"))
	}
	if err = config.Create(path, c); err != nil {
		return unknown(err)
	}
	fmt.Printf("Bound %s. Configuration: %s\nStart the connection: dune --config %s fabricd\n", result.Machine.Name, path, path)
	return nil
}
