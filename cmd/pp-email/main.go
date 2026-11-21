// Command pp-email sends personalized emails from CSV rows through s-nail.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"pp_email/internal/sendlimits"
	"pp_email/internal/smail"
	"pp_email/internal/tui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pp-email:", err)
		os.Exit(1)
	}
}

func run() error {
	path, err := exec.LookPath("s-nail")
	if err != nil {
		return errors.New("s-nail was not found on PATH")
	}
	directory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("find user config directory: %w", err)
	}
	limitsPath := filepath.Join(configDir, "pp-email", "limits.json")
	limits, err := sendlimits.Load(limitsPath)
	if err != nil {
		return fmt.Errorf("load send limits from %s: %w", limitsPath, err)
	}

	model := tui.New(smail.New(path, os.Environ()), directory, limits)
	program := tea.NewProgram(model, tea.WithoutSignalHandler())
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupts)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-interrupts:
				program.Send(tui.StopMsg{})
			}
		}
	}()
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run terminal interface: %w", err)
	}
	return nil
}
