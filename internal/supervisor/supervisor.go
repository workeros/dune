package supervisor

import (
	"context"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func Run(ctx context.Context, path string) error {
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	var wg sync.WaitGroup
	for _, name := range []string{"gateway", "fabricd"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			delay := 100 * time.Millisecond
			for ctx.Err() == nil {
				cmd := exec.Command(exe, "--config", path, name)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				start := time.Now()
				e := cmd.Start()
				if e == nil {
					log.Printf("supervisor started %s pid=%d", name, cmd.Process.Pid)
					done := make(chan error, 1)
					go func() { done <- cmd.Wait() }()
					select {
					case e = <-done:
					case <-ctx.Done():
						cmd.Process.Signal(syscall.SIGTERM)
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							cmd.Process.Kill()
							<-done
						}
						return
					}
				}
				if ctx.Err() != nil {
					return
				}
				if time.Since(start) > 30*time.Second {
					delay = 100 * time.Millisecond
				}
				log.Printf("supervisor %s exited (%v); restarting in %s", name, e, delay)
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
				delay *= 2
				if delay > 5*time.Second {
					delay = 5 * time.Second
				}
			}
		}(name)
	}
	wg.Wait()
	return nil
}
