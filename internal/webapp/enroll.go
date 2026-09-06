package webapp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/config"
)

func EnrollMachine(ctx context.Context, path, site, token, certificate string) error {
	u, err := url.Parse(site)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return fmt.Errorf("site must be an HTTP or HTTPS origin")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("machine binding requires HTTP or HTTPS")
	}
	if len(token) != 64 {
		return fmt.Errorf("one-time binding token required")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("configuration already exists; choose a separate --config path")
	} else if !os.IsNotExist(err) {
		return err
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
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(site, "/")+"/api/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dune-Request", "1")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var result struct {
		Machine    Machine `json:"machine"`
		Credential string  `json:"credential"`
		Gateway    string  `json:"gateway"`
		Error      string  `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 256*1024)).Decode(&result); err != nil {
		return fmt.Errorf("invalid binding response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("binding failed (%d): %s", response.StatusCode, result.Error)
	}
	c := config.Config{Gateway: result.Gateway, Token: result.Credential, Target: result.Machine.ID, Certificate: certificate, SessionDir: filepath.Join(filepath.Dir(path), "sessions")}
	if u.Scheme == "https" && !strings.HasPrefix(c.Gateway, "wss://") {
		return fmt.Errorf("server attempted to downgrade the machine connection")
	}
	if err = config.Create(path, c); err != nil {
		return fmt.Errorf("machine registered, but local config could not be saved; revoke the incomplete binding in the website before retrying: %w", err)
	}
	fmt.Printf("Bound %s. Configuration: %s\nStart the connection: dune --config %s fabricd\n", result.Machine.Name, path, path)
	return nil
}
