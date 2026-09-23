package runningprogram

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func openExecuting() (*os.File, error) { return os.Open("/proc/self/exe") }

func processStart() (string, error) {
	body, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(body), ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(body[end+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("missing kernel process start time")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", err
	}
	return fields[19], nil
}
