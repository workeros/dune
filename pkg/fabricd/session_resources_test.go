package fabricd

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func TestSessionResourcesFreezeOriginalInodesAndInstanceBeforeRegistration(t *testing.T) {
	id, instance := wire.ID(), wire.ID()
	directory := filepath.Join(t.TempDir(), id)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "dune-resource-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "ipc")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	reg := sessionRegistration{Instance: instance, Runtime: api.Runtime{ID: id}, Target: api.SubmissionTarget{RuntimeID: id}}
	if _, err := captureSessionResources(directory, socket, reg); err == nil {
		t.Fatal("missing instance marker was accepted")
	}
	if err := savePrivateFile(filepath.Join(directory, "instance.json"), instance); err != nil {
		t.Fatal(err)
	}
	resources, err := captureSessionResources(directory, socket, reg)
	if err != nil {
		t.Fatal(err)
	}
	if resources.Directory.Inode == 0 || resources.Socket.Inode == 0 || resources.Instance != instance {
		t.Fatal(resources)
	}
	if err := savePrivateFile(filepath.Join(directory, "instance.json"), wire.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := captureSessionResources(directory, socket, reg); err == nil {
		t.Fatal("reused path was accepted with another instance")
	}
	if err := savePrivateFile(filepath.Join(directory, "instance.json"), instance); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := captureSessionResources(directory, socket, reg); err == nil {
		t.Fatal("non-private resource was accepted")
	}
}
