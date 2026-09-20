package process

import (
	"fmt"
	"os"
	"strings"
)

func BootID() (string, error) {
	body, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(body))
	if len(id) != 36 {
		return "", fmt.Errorf("invalid kernel boot identity")
	}
	return id, nil
}
