// Package appstate locates the directory for pp-email's persistent state.
package appstate

import (
	"fmt"
	"os"
	"path/filepath"
)

func Directory() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find user state directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "pp-email"), nil
}
