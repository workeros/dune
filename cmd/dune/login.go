package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/pkg/login"
)

func loginClient(site, certificate string) (*login.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if certificate != "" {
		data, err := os.ReadFile(certificate)
		if err != nil {
			return nil, err
		}
		tc.RootCAs = x509.NewCertPool()
		if !tc.RootCAs.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("invalid login trust certificate")
		}
	}
	return login.New(login.Options{Site: site, TLSConfig: tc})
}

func runLogin(ctx context.Context, args []string, logout bool) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	file := flags.String("file", config.DefaultLoginPath(), "private CLI credential file")
	site := flags.String("site", "", "Dune HTTPS site URL including deployment prefix")
	certificate := flags.String("certificate", "", "optional custom trust certificate")
	noBrowser := flags.Bool("no-browser", false, "print the confirmation URL without opening a browser")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected login arguments")
	}
	if logout {
		var invalid string
		flags.Visit(func(f *flag.Flag) {
			if f.Name != "file" {
				invalid = f.Name
			}
		})
		if invalid != "" {
			return fmt.Errorf("logout does not accept --%s; it uses the saved login", invalid)
		}
	}
	absolute, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	*file = absolute
	unlock, err := config.LockUserCredentials(*file)
	if err != nil {
		return err
	}
	defer unlock()
	if logout {
		credentials, err := config.LoadUserCredentials(*file)
		if err != nil {
			return err
		}
		client, err := loginClient(credentials.Session.Site, credentials.Certificate)
		if err != nil {
			return err
		}
		defer client.Close()
		if err := client.Logout(ctx, credentials.Session); err != nil {
			return err
		}
		current, err := config.LoadUserCredentials(*file)
		if err != nil {
			return err
		}
		if current != credentials {
			return fmt.Errorf("credential file changed; retained the newer file")
		}
		if err := os.Remove(*file); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Dune CLI session revoked.")
		return nil
	}
	if _, err := os.Lstat(*file); !os.IsNotExist(err) {
		return fmt.Errorf("credential file already exists or cannot be checked; log out first or choose another --file")
	}
	if *certificate != "" {
		*certificate, err = filepath.Abs(*certificate)
		if err != nil {
			return err
		}
	}
	client, err := loginClient(*site, *certificate)
	if err != nil {
		return err
	}
	defer client.Close()
	attempt, err := client.Begin(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Open %s\nVerify code: %s\nConfirm only your own login request.\n", attempt.VerificationURL, attempt.Code)
	if !*noBrowser {
		name := "xdg-open"
		if runtime.GOOS == "darwin" {
			name = "open"
		}
		openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := exec.CommandContext(openCtx, name, attempt.VerificationURL).Run()
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Could not open a browser; open the URL above manually.")
		}
	}
	session, err := attempt.Wait(ctx)
	if err != nil {
		return err
	}
	if err := config.SaveUserCredentials(*file, config.UserCredentials{Session: session, Certificate: *certificate}); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Logout(cleanup, session)
		return fmt.Errorf("could not save private CLI credentials: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Signed in to %s as %s. Credentials: %s\n", session.Site, session.PrincipalID, *file)
	return nil
}
