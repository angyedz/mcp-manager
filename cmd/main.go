package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"

	"mcp-manager/internal/audit"
	"mcp-manager/internal/config"
	"mcp-manager/internal/installer"
	"mcp-manager/internal/process"
	"mcp-manager/internal/safe"
	"mcp-manager/internal/ui"

	tea "github.com/charmbracelet/bubbletea"
)

func exitWithError(err error) {
	fmt.Fprintf(os.Stderr, "\n[CRITICAL ERROR] %v\n", err)
	if runtime.GOOS == "windows" {
		fmt.Println("\nPress Enter to exit...")
		var dummy string
		_, _ = fmt.Scanln(&dummy)
	}
	os.Exit(1)
}

func main() {
	// Global Panic Recovery
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("recovered from panic: %v\n\nStack trace:\n%s", r, debug.Stack())
			exitWithError(err)
		}
	}()

	// 1. Initialize Viper Configuration
	if err := config.InitConfig(); err != nil {
		exitWithError(fmt.Errorf("failed to initialize config: %w", err))
	}

	// 2. Initialize Audit Logger
	if err := audit.InitLogger(); err != nil {
		exitWithError(fmt.Errorf("failed to initialize audit logger: %w", err))
	}

	// 3. Check and generate admin bearer token if missing
	adminToken, err := config.GetSecret("admin_token")
	if err != nil || adminToken == "" {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			exitWithError(fmt.Errorf("failed to generate secure random admin token: %w", err))
		}
		newToken := fmt.Sprintf("bearer_%x", b)
		if err := config.SaveSecret("admin_token", newToken); err != nil {
			exitWithError(fmt.Errorf("failed to save admin token: %w", err))
		}
	}

	// Check if other secrets exist in keyring
	hasSecrets := true
	secrets := []string{"ngrok_token"}
	for _, s := range secrets {
		val, err := config.GetSecret(s)
		if err != nil || val == "" {
			hasSecrets = false
			break
		}
	}

	// 4. Show Setup UI on first run if secrets are missing
	if !hasSecrets {
		p := tea.NewProgram(ui.NewSetupModel(), tea.WithAltScreen())
		if _, err := p.Run(); err != nil {
			exitWithError(fmt.Errorf("error running setup: %w", err))
		}

		// Re-verify secrets were saved after setup exits
		hasSecrets = true
		for _, s := range secrets {
			val, err := config.GetSecret(s)
			if err != nil || val == "" {
				hasSecrets = false
				break
			}
		}
		if !hasSecrets {
			exitWithError(fmt.Errorf("setup was not completed"))
		}
	}

	// 5. Ensure Node.js is installed (required for npm packages)
	if err := installer.EnsureNodeJS(); err != nil {
		exitWithError(fmt.Errorf("error ensuring Node.js: %w", err))
	}

	// 6. Ensure npm packages are installed
	if err := installer.EnsureMCPProxy(); err != nil {
		exitWithError(fmt.Errorf("error installing mcp-proxy: %w", err))
	}
	if err := installer.EnsureDesktopCommander(); err != nil {
		exitWithError(fmt.Errorf("error installing desktop-commander: %w", err))
	}

	// 7. Ensure ngrok binary is installed
	ngrokPath, _, err := installer.GetBinaryPaths()
	if err != nil {
		exitWithError(fmt.Errorf("error getting binary paths: %w", err))
	}
	ngrokURL, _, _, _ := installer.GetBinaryURLs()
	fmt.Println("Checking ngrok...")
	if err := installer.EnsureBinary("ngrok", ngrokURL, ngrokPath); err != nil {
		exitWithError(fmt.Errorf("error installing ngrok: %w", err))
	}

	// 8. Instantiate Process Manager
	pm := process.NewProcessManager()

	// Setup signal notification channel for graceful shutdown on SIGINT/SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	safe.Go(func() {
		<-sigChan
		fmt.Println("\nReceived shutdown signal. Cleaning up background processes...")
		pm.StopAll()
		os.Exit(0)
	})

	// 9. Start Main TUI Menu with Process Manager dependency
	p := ui.NewProgram(pm)
	_, err = p.Run()

	// Ensure all project processes are cleaned up on exit
	fmt.Println("Cleaning up background processes...")
	pm.StopAll()

	if err != nil {
		exitWithError(fmt.Errorf("error running program: %w", err))
	}
}




