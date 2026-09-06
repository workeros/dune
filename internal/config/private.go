package config

import (
	"fmt"
	"io"
	"os"
	"syscall"

	"gopkg.in/yaml.v3"
)

func privateYAML(path, label string, value any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() || info.Size() > 64*1024 {
		return fmt.Errorf("%s config must be a private, owned regular file of at most 64 KiB", label)
	}
	decoder := yaml.NewDecoder(io.LimitReader(f, 64*1024))
	decoder.KnownFields(true)
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid %s configuration", label)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fmt.Errorf("one %s configuration required", label)
	}
	return nil
}
