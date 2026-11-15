//go:build !unix

package cmd

import "errors"

func execLegacyRollback(string) error {
	return errors.New("transition recovery requires a Unix service")
}
