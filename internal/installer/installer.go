package installer

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var httpClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: func() http.RoundTripper {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSHandshakeTimeout = 45 * time.Second
		return t
	}(),
}

type progressWriter struct {
	total      int64
	downloaded int64
	lastPrint  time.Time
	name       string
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	pw.downloaded += int64(n)

	if time.Since(pw.lastPrint) > 500*time.Millisecond || pw.downloaded == pw.total {
		pw.lastPrint = time.Now()
		if pw.total > 0 {
			percent := float64(pw.downloaded) / float64(pw.total) * 100
			fmt.Printf("\r  Downloading %s: %.1f%% (%.2f/%.2f MB)...", pw.name, percent, float64(pw.downloaded)/(1024*1024), float64(pw.total)/(1024*1024))
		} else {
			fmt.Printf("\r  Downloading %s: %.2f MB...", pw.name, float64(pw.downloaded)/(1024*1024))
		}
	}
	return n, nil
}

// GetBinaryURLs returns download URLs and file extensions for Ngrok based on the target system
func GetBinaryURLs() (ngrokURL, cfURL, ngrokExt, cfExt string) {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	switch osName {
	case "windows":
		cfURL = ""
		cfExt = ""
		if arch == "arm64" {
			ngrokURL = "https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-windows-arm64.zip"
		} else {
			ngrokURL = "https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-windows-amd64.zip"
		}
		ngrokExt = ".zip"
	case "darwin":
		cfURL = ""
		cfExt = ""
		if arch == "arm64" {
			ngrokURL = "https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-darwin-arm64.zip"
		} else {
			ngrokURL = "https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-darwin-amd64.zip"
		}
		ngrokExt = ".zip"
	default: // Linux / others
		cfURL = ""
		cfExt = ""
		ngrokURL = "https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-linux-amd64.tgz"
		ngrokExt = ".tgz"
	}
	return
}

// GetBinaryPaths returns target file paths for ngrok
func GetBinaryPaths() (ngrokPath, cfPath string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}

	binDir := filepath.Join(home, ".config", "mcp-manager", "bin")

	ngrokName := "ngrok"
	if runtime.GOOS == "windows" {
		ngrokName += ".exe"
	}

	return filepath.Join(binDir, ngrokName), "", nil
}

// EnsureNodeJS checks if Node.js is installed. If not, downloads and installs it silently.
func EnsureNodeJS() error {
	fmt.Println("Checking Node.js...")

	// Check if node is already available in PATH
	cmd := exec.Command("node", "--version")
	if err := cmd.Run(); err == nil {
		fmt.Println("  Node.js is already installed.")
		return nil
	}

	fmt.Println("  Node.js not found. Downloading and installing...")

	switch runtime.GOOS {
	case "windows":
		return installNodeWindows()
	case "darwin":
		return installNodeMac()
	default:
		return installNodeLinux()
	}
}

func installNodeWindows() error {
	nodeURL := "https://nodejs.org/dist/v22.16.0/node-v22.16.0-x64.msi"
	if runtime.GOARCH == "arm64" {
		nodeURL = "https://nodejs.org/dist/v22.16.0/node-v22.16.0-arm64.msi"
	}

	tmpFile, err := os.CreateTemp("", "node-installer-*.msi")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Ensure cleanup is solid and file handle is closed first
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	fmt.Printf("  Downloading Node.js from %s...\n", nodeURL)
	resp, err := httpClient.Get(nodeURL)
	if err != nil {
		return fmt.Errorf("failed to download Node.js: %w", err)
	}
	defer resp.Body.Close()

	pw := &progressWriter{
		total: resp.ContentLength,
		name:  "Node.js installer",
	}

	if _, err := io.Copy(io.MultiWriter(tmpFile, pw), resp.Body); err != nil {
		return fmt.Errorf("failed to save installer: %w", err)
	}
	fmt.Println() // print newline after progress bar ends
	_ = tmpFile.Close()

	fmt.Println("  Installing Node.js (silent)...")
	installCmd := exec.Command("msiexec.exe", "/i", tmpPath, "/quiet", "/norestart")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("failed to install Node.js: %w", err)
	}

	fmt.Println("  Node.js installed successfully.")
	return nil
}

func installNodeMac() error {
	if _, err := exec.LookPath("brew"); err == nil {
		fmt.Println("  Installing Node.js via Homebrew...")
		cmd := exec.Command("brew", "install", "node")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("Node.js not found and Homebrew is not available. Please install Node.js from https://nodejs.org")
}

func installNodeLinux() error {
	if _, err := exec.LookPath("apt-get"); err == nil {
		fmt.Println("  Installing Node.js via apt-get...")
		setup := exec.Command("bash", "-c", "curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -")
		setup.Stdout = os.Stdout
		setup.Stderr = os.Stderr
		_ = setup.Run()
		cmd := exec.Command("sudo", "apt-get", "install", "-y", "nodejs")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("Node.js not found. Please install Node.js from https://nodejs.org")
}

// EnsureMCPProxy installs mcp-proxy globally via npm (skips if already installed)
func EnsureMCPProxy() error {
	fmt.Println("Checking mcp-proxy...")

	// Check if already installed
	var checkCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		checkCmd = exec.Command("cmd.exe", "/c", "npm", "list", "-g", "--depth=0", "mcp-proxy")
	} else {
		checkCmd = exec.Command("npm", "list", "-g", "--depth=0", "mcp-proxy")
	}
	if err := checkCmd.Run(); err == nil {
		fmt.Println("  mcp-proxy already installed, skipping.")
		return nil
	}

	fmt.Println("  Installing mcp-proxy globally...")
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd.exe", "/c", "npm", "install", "-g", "mcp-proxy")
	} else {
		cmd = exec.Command("npm", "install", "-g", "mcp-proxy")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install mcp-proxy: %w", err)
	}
	fmt.Println("  mcp-proxy installed.")
	return nil
}

// EnsureDesktopCommander installs @wonderwhy-er/desktop-commander globally via npm (skips if already installed)
func EnsureDesktopCommander() error {
	fmt.Println("Checking @wonderwhy-er/desktop-commander...")

	// Check if already installed
	var checkCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		checkCmd = exec.Command("cmd.exe", "/c", "npm", "list", "-g", "--depth=0", "@wonderwhy-er/desktop-commander")
	} else {
		checkCmd = exec.Command("npm", "list", "-g", "--depth=0", "@wonderwhy-er/desktop-commander")
	}
	if err := checkCmd.Run(); err == nil {
		fmt.Println("  @wonderwhy-er/desktop-commander already installed, skipping.")
		return nil
	}

	fmt.Println("  Installing @wonderwhy-er/desktop-commander globally...")
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd.exe", "/c", "npm", "install", "-g", "@wonderwhy-er/desktop-commander")
	} else {
		cmd = exec.Command("npm", "install", "-g", "@wonderwhy-er/desktop-commander")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install desktop-commander: %w", err)
	}
	fmt.Println("  @wonderwhy-er/desktop-commander installed.")
	return nil
}

// EnsureBinary checks if a binary exists at destPath. If not, it downloads from url and extracts/installs it.
func EnsureBinary(name, url, destPath string) error {
	// 1. Check if binary already exists with a valid size
	if info, err := os.Stat(destPath); err == nil && info.Size() > 0 {
		return nil
	}

	fmt.Printf("Downloading %s from %s...\n", name, url)

	// Create destPath directory if it doesn't exist
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", destDir, err)
	}

	// 2. Download from URL
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	// Create a temp file path in the same directory as destPath
	// to ensure it's on the same partition/drive, making os.Rename atomic and fast.
	tempDestPath := destPath + ".tmp"
	defer func() {
		// Clean up the temp file if it's still around
		_ = os.Remove(tempDestPath)
	}()

	// Determine if it needs extraction based on URL suffix
	if strings.HasSuffix(url, ".zip") {
		// Save to a temp file first since zip reader needs ReaderAt (random access)
		tmpFile, err := os.CreateTemp("", "mcp-installer-zip-*")
		if err != nil {
			return fmt.Errorf("failed to create temp file: %w", err)
		}
		tmpFileName := tmpFile.Name()
		defer func() {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFileName)
		}()

		pw := &progressWriter{
			total: resp.ContentLength,
			name:  name + " archive",
		}
		_, copyErr := io.Copy(io.MultiWriter(tmpFile, pw), resp.Body)
		_ = tmpFile.Close() // Close immediately so Windows releases the file lock
		fmt.Println()       // print newline after progress bar ends

		if copyErr != nil {
			return fmt.Errorf("failed to save temp file: %w", copyErr)
		}

		// Read zip
		zr, err := zip.OpenReader(tmpFileName)
		if err != nil {
			return fmt.Errorf("failed to open zip: %w", err)
		}
		defer zr.Close()

		found := false
		for _, f := range zr.File {
			// Find ngrok or ngrok.exe
			baseName := filepath.Base(f.Name)
			if baseName == name || baseName == name+".exe" {
				rc, err := f.Open()
				if err != nil {
					return fmt.Errorf("failed to open zipped file: %w", err)
				}

				out, err := os.OpenFile(tempDestPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
				if err != nil {
					_ = rc.Close()
					return fmt.Errorf("failed to create destination temp file: %w", err)
				}

				_, copyErr := io.Copy(out, rc)
				_ = out.Close()
				_ = rc.Close()

				if copyErr != nil {
					return fmt.Errorf("failed to extract file: %w", copyErr)
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("binary %s not found in zip archive", name)
		}

	} else if strings.HasSuffix(url, ".tar.gz") || strings.HasSuffix(url, ".tgz") {
		pw := &progressWriter{
			total: resp.ContentLength,
			name:  name + " archive",
		}
		progressReader := io.TeeReader(resp.Body, pw)
		gr, err := gzip.NewReader(progressReader)
		if err != nil {
			fmt.Println()
			return fmt.Errorf("failed to read gzip: %w", err)
		}
		defer gr.Close()

		tr := tar.NewReader(gr)
		found := false
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				fmt.Println()
				return fmt.Errorf("failed to read tar: %w", err)
			}

			baseName := filepath.Base(hdr.Name)
			if baseName == name {
				out, err := os.OpenFile(tempDestPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
				if err != nil {
					fmt.Println()
					return fmt.Errorf("failed to create destination temp file: %w", err)
				}

				_, copyErr := io.Copy(out, tr)
				_ = out.Close()

				if copyErr != nil {
					fmt.Println()
					return fmt.Errorf("failed to extract file: %w", copyErr)
				}
				found = true
				break
			}
		}
		fmt.Println() // print newline after progress bar ends
		if !found {
			return fmt.Errorf("binary %s not found in tar archive", name)
		}

	} else {
		// Direct file copy
		out, err := os.OpenFile(tempDestPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("failed to create destination temp file: %w", err)
		}

		pw := &progressWriter{
			total: resp.ContentLength,
			name:  name,
		}
		_, copyErr := io.Copy(io.MultiWriter(out, pw), resp.Body)
		_ = out.Close()
		fmt.Println() // print newline after progress bar ends

		if copyErr != nil {
			return fmt.Errorf("failed to save file: %w", copyErr)
		}
	}

	// 3. Set executable permission on Unix-based systems
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tempDestPath, 0755); err != nil {
			return fmt.Errorf("failed to set chmod +x on temp: %w", err)
		}
	}

	// 4. Rename temp file to destination path atomically
	if _, err := os.Stat(destPath); err == nil {
		_ = os.Remove(destPath) // Delete existing first on Windows to allow overwrite
	}

	if err := os.Rename(tempDestPath, destPath); err != nil {
		return fmt.Errorf("failed to rename temp file to final destination: %w", err)
	}

	return nil
}



