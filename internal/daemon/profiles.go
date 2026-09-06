package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func (d *Daemon) agentConfig(req api.AgentConfigRequest) (any, error) {
	if d.stateDir == "" {
		return nil, &api.Error{Code: "UNSUPPORTED", Detail: "saved Agent configurations require an session directory"}
	}
	d.profileMu.Lock()
	defer d.profileMu.Unlock()
	path := filepath.Join(d.stateDir, "agents.json")
	profiles := map[string]api.AgentConfig{}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		dec := json.NewDecoder(io.LimitReader(f, 1024*1024))
		dec.DisallowUnknownFields()
		err = dec.Decode(&profiles)
		if err == nil {
			var extra any
			if dec.Decode(&extra) != io.EOF {
				err = fmt.Errorf("invalid trailing Agent config data")
			}
		}
		f.Close()
		if err != nil || profiles == nil {
			return nil, fmt.Errorf("cannot read saved Agent configurations")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var result any
	switch req.Action {
	case "list":
		out := make([]api.AgentConfig, 0, len(profiles))
		for _, config := range profiles {
			out = append(out, config)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	case "save":
		if req.Config == nil {
			return nil, fmt.Errorf("config required")
		}
		config := *req.Config
		config.Name = strings.TrimSpace(config.Name)
		if config.Name == "" || len(config.Name) > 120 || strings.ContainsFunc(config.Name, unicode.IsControl) {
			return nil, fmt.Errorf("Agent name must be 1..120 bytes without control characters")
		}
		if err := config.Profile("/").Validate(); err != nil {
			return nil, err
		}
		if config.ID == "" {
			if len(profiles) >= 64 {
				return nil, fmt.Errorf("at most 64 saved Agent configurations")
			}
			config.ID = wire.ID()
		} else if _, ok := profiles[config.ID]; !ok {
			return nil, fmt.Errorf("Agent configuration not found")
		}
		profiles[config.ID] = config
		result = config
	case "delete":
		if _, ok := profiles[req.ID]; !ok {
			return nil, fmt.Errorf("Agent configuration not found")
		}
		delete(profiles, req.ID)
	default:
		return nil, fmt.Errorf("Agent configuration action must be list/save/delete")
	}
	b, err := json.Marshal(profiles)
	if err != nil {
		return nil, err
	}
	if len(b) > 1024*1024 {
		return nil, fmt.Errorf("Agent configuration capacity exceeded")
	}
	f, err = os.CreateTemp(d.stateDir, ".agents-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return nil, err
	}
	return result, nil
}
