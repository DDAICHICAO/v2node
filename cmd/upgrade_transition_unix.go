//go:build unix

package cmd

import (
	"os"
	"syscall"
)

func execLegacyRollback(path string) error {
	return syscall.Exec(path, append([]string{path}, os.Args[1:]...), os.Environ())
}
