package fabricd

import "golang.org/x/sys/unix"

func renameCleanupResource(parent int, name, quarantine string) error {
	return unix.Renameat2(parent, name, parent, quarantine, unix.RENAME_NOREPLACE)
}
