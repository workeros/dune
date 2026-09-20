package fabricd

import "golang.org/x/sys/unix"

func renameCleanupResource(parent int, name, quarantine string) error {
	return unix.RenameatxNp(parent, name, parent, quarantine, unix.RENAME_EXCL)
}
