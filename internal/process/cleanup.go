package process

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// Cleaner holds only temporary upload paths. Its pipe closes when fabricd dies,
// including SIGKILL; no disk ledger or filesystem scan is needed after restart.
type Cleaner struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	in, out *os.File
	enc     *json.Encoder
	dec     *json.Decoder
}
type cleanupRequest struct {
	Directory string
	Prefix    string
	Release   string
}
type cleanupReply struct {
	Path  string
	Error string
}

func NewCleaner() (*Cleaner, error) {
	exe, e := os.Executable()
	if e != nil {
		return nil, e
	}
	inR, inW, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	outR, outW, e := os.Pipe()
	if e != nil {
		inR.Close()
		inW.Close()
		return nil, e
	}
	cmd := exec.Command(exe, "_cleanup")
	cmd.ExtraFiles = []*os.File{inR, outW}
	if e = cmd.Start(); e != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, e
	}
	inR.Close()
	outW.Close()
	return &Cleaner{cmd: cmd, in: inW, out: outR, enc: json.NewEncoder(inW), dec: json.NewDecoder(outR)}, nil
}
func (c *Cleaner) request(r cleanupRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.enc.Encode(r); e != nil {
		return "", e
	}
	var reply cleanupReply
	if e := c.dec.Decode(&reply); e != nil {
		return "", e
	}
	if reply.Error != "" {
		return "", fmt.Errorf("%s", reply.Error)
	}
	return reply.Path, nil
}
func (c *Cleaner) Create(dir, prefix string) (string, error) {
	return c.request(cleanupRequest{Directory: dir, Prefix: prefix})
}
func (c *Cleaner) Release(path string) { _, _ = c.request(cleanupRequest{Release: path}) }
func (c *Cleaner) Close()              { c.in.Close(); c.cmd.Wait(); c.out.Close() }
func CleanupGuard() int {
	in := os.NewFile(3, "owner")
	out := os.NewFile(4, "reply")
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	paths := map[string]bool{}
	defer func() {
		for p := range paths {
			os.Remove(p)
		}
	}()
	for {
		var r cleanupRequest
		if dec.Decode(&r) != nil {
			return 0
		}
		var reply cleanupReply
		if r.Release != "" {
			delete(paths, r.Release)
		} else if len(paths) >= 64 {
			reply.Error = "upload temporary file limit"
		} else {
			f, e := os.CreateTemp(r.Directory, r.Prefix)
			if e != nil {
				reply.Error = e.Error()
			} else {
				reply.Path = f.Name()
				paths[f.Name()] = true
				f.Close()
			}
		}
		if enc.Encode(reply) != nil {
			return 0
		}
	}
}
