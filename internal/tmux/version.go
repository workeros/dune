package tmux

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/aiomni/dune/pkg/api"
)

// Versions never starts a server. -V identifies only the client executable;
// #{version}/#{pid} are evaluated by the existing server over its own socket.
func (s *Server) Versions(ctx context.Context) api.TmuxVersion {
	info := api.TmuxVersion{CheckedAt: time.Now().UTC(), ServerState: "unavailable"}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client := exec.CommandContext(ctx, s.Binary, "-V")
	client.WaitDelay = 100 * time.Millisecond
	client.Env = clientEnv()
	clientOut := &output{limit: 256}
	client.Stdout, client.Stderr = clientOut, &output{limit: 1024}
	if client.Run() != nil || !strings.HasPrefix(clientOut.String(), "tmux ") || !validVersion(strings.TrimSpace(strings.TrimPrefix(clientOut.String(), "tmux "))) {
		info.ErrorCode = "TMUX_CLIENT_UNAVAILABLE"
		return info
	}
	info.ClientVersion = strings.TrimSpace(strings.TrimPrefix(clientOut.String(), "tmux "))
	file, err := os.Lstat(s.Socket)
	if os.IsNotExist(err) {
		info.ServerState = "absent"
		return info
	}
	if err != nil {
		info.ErrorCode = "TMUX_SOCKET_UNAVAILABLE"
		return info
	}
	owner, ok := file.Sys().(*syscall.Stat_t)
	if file.Mode()&os.ModeSocket == 0 || !ok || int(owner.Uid) != os.Getuid() || file.Mode().Perm()&0077 != 0 {
		info.ErrorCode = "TMUX_SOCKET_INVALID"
		return info
	}
	server := exec.CommandContext(ctx, s.Binary, "-N", "-S", s.Socket, "display-message", "-p", "#{pid}\t#{version}")
	server.WaitDelay = 100 * time.Millisecond
	server.Env = clientEnv()
	serverOut := &output{limit: 256}
	server.Stdout, server.Stderr = serverOut, &output{limit: 1024}
	if server.Run() != nil {
		info.ErrorCode = "TMUX_SERVER_UNAVAILABLE"
		return info
	}
	pid, version, ok := strings.Cut(strings.TrimSpace(serverOut.String()), "\t")
	number, err := strconv.Atoi(pid)
	if !ok || err != nil || number <= 1 || !validVersion(version) {
		info.ErrorCode = "TMUX_SERVER_INVALID"
		return info
	}
	info.ServerVersion, info.ServerPID, info.ServerState = version, number, "running"
	return info
}

func validVersion(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsFunc(value, unicode.IsControl)
}
