package tests

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/testcert"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/webapp"
	"gopkg.in/yaml.v3"
)

func TestPostgresClusterWebProcesses(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	dir := t.TempDir()
	writeYAML := func(path string, value any) {
		t.Helper()
		data, err := yaml.Marshal(value)
		must(t, err)
		must(t, os.WriteFile(path, data, 0600))
	}
	databaseFile := filepath.Join(dir, "database.yaml")
	writeYAML(databaseFile, map[string]any{"postgres": map[string]string{"url": postgresWorkbenchURL(t, database)}})
	ca := testcert.New(t)
	sites := make([]string, 3)
	for i := range sites {
		instanceDir := filepath.Join(dir, fmt.Sprint(i))
		must(t, os.Mkdir(instanceDir, 0700))
		public, err := net.Listen("tcp", "127.0.0.1:0")
		must(t, err)
		peer, err := net.Listen("tcp", "127.0.0.1:0")
		must(t, err)
		publicAddress, peerAddress := public.Addr().String(), peer.Addr().String()
		sites[i] = "http://" + publicAddress + "/dune/"
		cert := ca.Issue(t, "127.0.0.1", nil)
		key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
		must(t, err)
		for name, data := range map[string][]byte{
			"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
			"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
			"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate.Raw}),
		} {
			must(t, os.WriteFile(filepath.Join(instanceDir, name), data, 0600))
		}
		clusterFile := filepath.Join(instanceDir, "cluster.yaml")
		writeYAML(clusterFile, map[string]any{"peer": map[string]string{"listen": peerAddress, "address": "https://" + peerAddress + "/private/peer", "certificate": "cert.pem", "key": "key.pem", "ca": "ca.pem"}})
		localFile := filepath.Join(instanceDir, "local.yaml")
		must(t, config.Create(localFile, config.Config{Gateway: "ws://" + publicAddress + "/dune/tunnel", Listen: publicAddress, Token: strings.Repeat("x", 32), Target: "unused-local-token"}))
		log, err := os.Create(filepath.Join(instanceDir, "web.log"))
		must(t, err)
		defer log.Close()
		must(t, public.Close())
		if i == 0 {
			// Peer remains occupied. Startup must fail and release the public
			// socket that it acquired before discovering this conflict.
			failed := exec.CommandContext(ctx, binary, "--config", localFile, "web", "--url", sites[i], "--database-config", databaseFile, "--cluster-config", clusterFile)
			output, err := failed.CombinedOutput()
			if err == nil || !strings.Contains(string(output), peerAddress) {
				t.Fatal("occupied peer port did not fail cluster startup")
			}
			released, err := net.Listen("tcp", publicAddress)
			must(t, err)
			must(t, released.Close())
			failed = exec.CommandContext(ctx, binary, "--config", localFile, "web", "--cluster-config", clusterFile)
			output, err = failed.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "requires a PostgreSQL") {
				t.Fatal("cluster Web process accepted the default SQLite backend")
			}
		}
		must(t, peer.Close())
		process := launchHostTestProcess(t, log, "--config", localFile, "web", "--url", sites[i], "--database-config", databaseFile, "--cluster-config", clusterFile)
		defer process.stop(t, syscall.SIGTERM)
		t.Cleanup(func() {
			data, _ := os.ReadFile(log.Name())
			if strings.Contains(string(data), "DATA RACE") {
				t.Error("cluster process reported a data race")
			}
		})
		probe := &http.Client{Timeout: time.Second}
		for {
			response, err := probe.Get(sites[i] + "api/bootstrap")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == 200 {
					break
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal("cluster Web process did not start")
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	jar, err := cookiejar.New(nil)
	must(t, err)
	browser := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	request := func(site, method, route string, body, out any) {
		t.Helper()
		data, err := json.Marshal(body)
		must(t, err)
		req, err := http.NewRequestWithContext(ctx, method, site+route, bytes.NewReader(data))
		must(t, err)
		origin, err := url.Parse(site)
		must(t, err)
		req.Header.Set("Origin", origin.Scheme+"://"+origin.Host)
		req.Header.Set("X-Dune-Request", "1")
		req.Header.Set("Content-Type", "application/json")
		response, err := browser.Do(req)
		must(t, err)
		defer response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 201 {
			t.Fatalf("cluster %s %s: %d", method, route, response.StatusCode)
		}
		if out != nil {
			must(t, json.NewDecoder(response.Body).Decode(out))
		}
	}
	credentials := map[string]string{"email": "cluster-web@example.test", "password": "cluster-process-password"}
	request(sites[0], "POST", "api/auth/register", credentials, nil)
	var enrollment struct{ Token string }
	request(sites[0], "POST", "api/enrollments", map[string]string{"name": "cluster machine"}, &enrollment)
	machineFile := filepath.Join(dir, "machine", "config.yaml")
	// Enrollment itself is consumed on B after being issued by A.
	must(t, webapp.EnrollMachine(ctx, machineFile, sites[1], enrollment.Token, ""))
	machine, err := config.Load(machineFile)
	must(t, err)
	log, err := os.Create(filepath.Join(dir, "fabricd.log"))
	must(t, err)
	defer log.Close()
	connector := launchHostTestProcess(t, log, "--config", machineFile, "fabricd")
	defer func() {
		connector.stop(t, syscall.SIGTERM)
		manager, err := tmux.Open(machine.SessionDir)
		if err == nil {
			manager.Close()
		}
	}()
	for _, entry := range []int{0, 2} {
		site := sites[entry]
		if entry != 0 {
			request(site, "POST", "api/auth/login", credentials, nil)
		}
		for {
			var page struct {
				Items []struct {
					ID     string
					Online bool
				}
			}
			request(site, "GET", "api/machines", nil, &page)
			if len(page.Items) == 1 && page.Items[0].ID == machine.Target && page.Items[0].Online {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("remote owner did not become visible")
			case <-time.After(25 * time.Millisecond):
			}
		}
		request(site, "POST", "api/machines/"+machine.Target+"/call", map[string]any{"operation": "runtime.list", "payload": struct{}{}}, nil)
		// A different entry revokes the shared browser session.
		request(sites[1], "POST", "api/auth/logout", struct{}{}, nil)
	}
}
