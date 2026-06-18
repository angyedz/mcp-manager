package process

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"mcp-manager/internal/config"
	"mcp-manager/internal/gateway"
	"mcp-manager/internal/installer"
	"mcp-manager/internal/safe"
)

type ProjectProcesses struct {
	ProxyCmd      *exec.Cmd
	GatewayServer *http.Server
	TunnelCmd     *exec.Cmd
	PublicURL     string
	IsStopping    bool
}

type ProcessManager struct {
	activeProcesses map[string]*ProjectProcesses
	mu              sync.Mutex
}

func NewProcessManager() *ProcessManager {
	return &ProcessManager{
		activeProcesses: make(map[string]*ProjectProcesses),
	}
}

// GetRunningProjects returns a list of names of all currently running projects
func (pm *ProcessManager) GetRunningProjects() []string {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var running []string
	for name := range pm.activeProcesses {
		running = append(running, name)
	}
	return running
}

// GetPublicURL returns the public URL for a running project
func (pm *ProcessManager) GetPublicURL(name string) string {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	proc, ok := pm.activeProcesses[name]
	if !ok {
		return ""
	}
	return proc.PublicURL
}

func runCmd(dir string, name string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		fullArgs := append([]string{"/c", name}, args...)
		cmd = exec.Command("cmd.exe", fullArgs...)
	} else {
		cmd = exec.Command(name, args...)
	}
	cmd.Dir = dir
	return cmd
}

func pollNgrokAPI() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
			resp, err := http.Get("http://localhost:4040/api/tunnels")
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}

			var data struct {
				Tunnels []struct {
					PublicURL string `json:"public_url"`
				} `json:"tunnels"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&data)
			resp.Body.Close()
			if decodeErr != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}

			if len(data.Tunnels) > 0 {
				return data.Tunnels[0].PublicURL, nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func (pm *ProcessManager) StartProject(project config.ProjectConfig) (string, error) {
	pm.mu.Lock()
	if _, exists := pm.activeProcesses[project.Name]; exists {
		pm.mu.Unlock()
		return "", fmt.Errorf("project %s is already running", project.Name)
	}
	pm.mu.Unlock()

	// 1. Fetch credentials
	ngrokToken, _ := config.GetSecret("ngrok_token")
	adminToken, _ := config.GetSecret("admin_token")

	// Free ports 3000 and 8000 first to prevent address-already-in-use errors from zombie processes
	FreePort(3000)
	FreePort(8000)

	// 2. Start MCP proxy process
	var proxyCmd *exec.Cmd

	if runtime.GOOS == "windows" {
		proxyCmd = runCmd(project.Path, "npx", "-y", "mcp-proxy", "--port", "3000", "--", "cmd.exe", "/c", "npx", "-y", "@wonderwhy-er/desktop-commander")
	} else {
		proxyCmd = runCmd(project.Path, "npx", "-y", "mcp-proxy", "--port", "3000", "--", "npx", "-y", "@wonderwhy-er/desktop-commander")
	}

	if err := proxyCmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start unified MCP proxy: %w", err)
	}
	safe.Go(func() {
		_ = proxyCmd.Wait()
	})

	// 3. Start Auth Gateway
	router := gateway.NewMCPRouter(adminToken)
	gwServer := &http.Server{
		Addr:    ":8000",
		Handler: router,
	}

	safe.Go(func() {
		currentServer := gwServer
		for {
			err := currentServer.ListenAndServe()

			// Sleep a bit before checking/restarting to avoid hot-looping
			time.Sleep(1 * time.Second)

			pm.mu.Lock()
			proc, exists := pm.activeProcesses[project.Name]
			if !exists || proc.IsStopping {
				pm.mu.Unlock()
				return
			}
			pm.mu.Unlock()

			if err == http.ErrServerClosed {
				return
			}

			fmt.Printf("[gateway] Gateway server exited: %v. Recreating...\n", err)
			FreePort(8000)

			pm.mu.Lock()
			newServer := &http.Server{
				Addr:    ":8000",
				Handler: router,
			}
			// Recheck exists in case project was stopped while we were FreePort'ing
			proc, exists = pm.activeProcesses[project.Name]
			if exists && !proc.IsStopping {
				proc.GatewayServer = newServer
			}
			currentServer = newServer
			pm.mu.Unlock()
		}
	})

	// Allow servers time to warm up
	time.Sleep(1 * time.Second)
	_ = router.DiscoverTools()

	// 4. Start Tunnel
	var tunnelCmd *exec.Cmd
	var publicURL string

	ngrokPath, cfPath, err := installer.GetBinaryPaths()
	if err != nil {
		killProcessTree(proxyCmd.Process)
		_ = gwServer.Close()
		return "", fmt.Errorf("failed to resolve tunnel paths: %w", err)
	}

	if project.TunnelType == "ngrok" {
		tunnelCmd = exec.Command(ngrokPath, "http", "8000", "--log", "stdout")
		tunnelCmd.Env = append(os.Environ(), "NGROK_AUTHTOKEN="+ngrokToken)

		stdoutPipe, err := tunnelCmd.StdoutPipe()
		if err != nil {
			killProcessTree(proxyCmd.Process)
			_ = gwServer.Close()
			return "", err
		}

		if err := tunnelCmd.Start(); err != nil {
			killProcessTree(proxyCmd.Process)
			_ = gwServer.Close()
			return "", fmt.Errorf("failed to start ngrok: %w", err)
		}
		safe.Go(func() {
			_ = tunnelCmd.Wait()
		})

		urlChan := make(chan string, 1)
		safe.Go(func() {
			scanner := bufio.NewScanner(stdoutPipe)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, "url=https://") {
					parts := strings.Split(line, "url=")
					if len(parts) > 1 {
						urlVal := strings.Fields(parts[1])[0]
						urlChan <- urlVal
						return
					}
				}
			}
		})

		select {
		case urlVal := <-urlChan:
			publicURL = urlVal
		case <-time.After(5 * time.Second):
			// Fallback: Query Ngrok's local API
			val, err := pollNgrokAPI()
			if err == nil && val != "" {
				publicURL = val
			} else {
				killProcessTree(proxyCmd.Process)
				_ = gwServer.Close()
				killProcessTree(tunnelCmd.Process)
				return "", fmt.Errorf("ngrok did not provide a public URL")
			}
		}

	} else { // cloudflare
		tunnelCmd = exec.Command(cfPath, "tunnel", "--url", "http://localhost:8000")
		stderrPipe, err := tunnelCmd.StderrPipe()
		if err != nil {
			killProcessTree(proxyCmd.Process)
			_ = gwServer.Close()
			return "", err
		}

		if err := tunnelCmd.Start(); err != nil {
			killProcessTree(proxyCmd.Process)
			_ = gwServer.Close()
			return "", fmt.Errorf("failed to start cloudflared: %w", err)
		}
		safe.Go(func() {
			_ = tunnelCmd.Wait()
		})

		urlChan := make(chan string, 1)
		safe.Go(func() {
			scanner := bufio.NewScanner(stderrPipe)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, "trycloudflare.com") {
					fields := strings.Fields(line)
					for _, f := range fields {
						if strings.Contains(f, "trycloudflare.com") {
							urlVal := strings.TrimSpace(f)
							urlVal = strings.Trim(urlVal, "| \t\r\n")
							if strings.HasPrefix(urlVal, "https://") {
								urlChan <- urlVal
								return
							}
						}
					}
				}
			}
		})

		select {
		case urlVal := <-urlChan:
			publicURL = urlVal
		case <-time.After(15 * time.Second):
			killProcessTree(proxyCmd.Process)
			_ = gwServer.Close()
			killProcessTree(tunnelCmd.Process)
			return "", fmt.Errorf("cloudflared did not provide a public URL")
		}
	}

	pm.mu.Lock()
	pm.activeProcesses[project.Name] = &ProjectProcesses{
		ProxyCmd:      proxyCmd,
		GatewayServer: gwServer,
		TunnelCmd:     tunnelCmd,
		PublicURL:     publicURL,
		IsStopping:    false,
	}
	pm.mu.Unlock()

	safe.Go(func() {
		pm.startWatchdog(project.Name, project)
	})

	return publicURL, nil
}

func (pm *ProcessManager) StopProject(projectName string) error {
	pm.mu.Lock()
	proc, exists := pm.activeProcesses[projectName]
	if !exists {
		pm.mu.Unlock()
		return fmt.Errorf("project %s is not running", projectName)
	}

	proc.IsStopping = true
	pm.mu.Unlock()

	// Forcefully kill process trees (including all grandchildren)
	if proc.ProxyCmd != nil {
		killProcessTree(proc.ProxyCmd.Process)
	}
	if proc.GatewayServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = proc.GatewayServer.Shutdown(ctx)
		cancel()
	}
	if proc.TunnelCmd != nil {
		killProcessTree(proc.TunnelCmd.Process)
	}

	pm.mu.Lock()
	delete(pm.activeProcesses, projectName)
	pm.mu.Unlock()
	return nil
}

// StopAll stops all active project processes.
func (pm *ProcessManager) StopAll() {
	pm.mu.Lock()
	var names []string
	for name := range pm.activeProcesses {
		names = append(names, name)
	}
	pm.mu.Unlock()

	for _, name := range names {
		_ = pm.StopProject(name)
	}
}

func (pm *ProcessManager) startWatchdog(projectName string, project config.ProjectConfig) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Track startup time to give a grace period before checking ports
	startTime := time.Now()
	unresponsiveCount := 0
	consecutiveProxyRestarts := 0
	consecutiveTunnelRestarts := 0

	ngrokToken, _ := config.GetSecret("ngrok_token")
	ngrokPath, _, _ := installer.GetBinaryPaths()

	for {
		select {
		case <-ticker.C:
			pm.mu.Lock()
			proc, exists := pm.activeProcesses[projectName]
			if !exists || proc.IsStopping {
				pm.mu.Unlock()
				return
			}

			// 1. Monitor ProxyCmd (mcp-proxy)
			needProxyRestart := false
			if proc.ProxyCmd != nil && proc.ProxyCmd.ProcessState != nil {
				// Proxy exited
				fmt.Printf("[watchdog] Proxy process exited. Restarting...\n")
				needProxyRestart = true
			} else if time.Since(startTime) > 15*time.Second {
				// Check if port is responding (only after startup grace period)
				if !isPortOpen("127.0.0.1:3000") {
					unresponsiveCount++
					fmt.Printf("[watchdog] Proxy port 3000 unresponsive count: %d/3\n", unresponsiveCount)
					if unresponsiveCount >= 3 {
						fmt.Printf("[watchdog] Proxy port 3000 unresponsive for 15 seconds. Restarting...\n")
						needProxyRestart = true
					}
				} else {
					unresponsiveCount = 0
					// Reset proxy failure count if it was successfully running & responsive
					consecutiveProxyRestarts = 0
				}
			}

			if needProxyRestart {
				consecutiveProxyRestarts++
				backoff := getBackoffDelay(consecutiveProxyRestarts)
				if backoff > 0 {
					fmt.Printf("[watchdog] Proxy restart exponential backoff: sleeping %v...\n", backoff)
					// Unlock during sleep to prevent freezing the entire UI/manager operations
					pm.mu.Unlock()
					time.Sleep(backoff)
					pm.mu.Lock()
					// Re-verify after sleep
					proc, exists = pm.activeProcesses[projectName]
					if !exists || proc.IsStopping {
						pm.mu.Unlock()
						return
					}
				}

				if proc.ProxyCmd != nil {
					killProcessTree(proc.ProxyCmd.Process)
				}
				FreePort(3000)

				var newProxyCmd *exec.Cmd
				if runtime.GOOS == "windows" {
					newProxyCmd = runCmd(project.Path, "npx", "-y", "mcp-proxy", "--port", "3000", "--", "cmd.exe", "/c", "npx", "-y", "@wonderwhy-er/desktop-commander")
				} else {
					newProxyCmd = runCmd(project.Path, "npx", "-y", "mcp-proxy", "--port", "3000", "--", "npx", "-y", "@wonderwhy-er/desktop-commander")
				}

				if err := newProxyCmd.Start(); err != nil {
					fmt.Printf("[watchdog] Failed to restart proxy: %v\n", err)
				} else {
					proc.ProxyCmd = newProxyCmd
					safe.Go(func() {
						_ = newProxyCmd.Wait()
					})
					fmt.Printf("[watchdog] Proxy restarted successfully.\n")
					startTime = time.Now()
					unresponsiveCount = 0
				}
			}

			// 2. Monitor TunnelCmd (ngrok / cloudflare)
			if proc.TunnelCmd != nil && proc.TunnelCmd.ProcessState != nil {
				consecutiveTunnelRestarts++
				backoff := getBackoffDelay(consecutiveTunnelRestarts)
				if backoff > 0 {
					fmt.Printf("[watchdog] Tunnel restart exponential backoff: sleeping %v...\n", backoff)
					pm.mu.Unlock()
					time.Sleep(backoff)
					pm.mu.Lock()
					// Re-verify after sleep
					proc, exists = pm.activeProcesses[projectName]
					if !exists || proc.IsStopping {
						pm.mu.Unlock()
						return
					}
				}

				fmt.Printf("[watchdog] Tunnel process exited. Restarting...\n")
				killProcessTree(proc.TunnelCmd.Process)

				var newTunnelCmd *exec.Cmd
				var stdoutPipe io.ReadCloser
				var stderrPipe io.ReadCloser
				var err error

				if project.TunnelType == "ngrok" {
					newTunnelCmd = exec.Command(ngrokPath, "http", "8000", "--log", "stdout")
					newTunnelCmd.Env = append(os.Environ(), "NGROK_AUTHTOKEN="+ngrokToken)
					stdoutPipe, err = newTunnelCmd.StdoutPipe()
				} else {
					_, cfPath, _ := installer.GetBinaryPaths()
					newTunnelCmd = exec.Command(cfPath, "tunnel", "--url", "http://localhost:8000")
					stderrPipe, err = newTunnelCmd.StderrPipe()
				}

				if err != nil {
					fmt.Printf("[watchdog] Failed to get tunnel pipe: %v\n", err)
				} else if err := newTunnelCmd.Start(); err != nil {
					fmt.Printf("[watchdog] Failed to restart tunnel: %v\n", err)
				} else {
					proc.TunnelCmd = newTunnelCmd
					safe.Go(func() {
						_ = newTunnelCmd.Wait()
					})

					urlChan := make(chan string, 1)
					if project.TunnelType == "ngrok" {
						safe.Go(func() {
							scanner := bufio.NewScanner(stdoutPipe)
							for scanner.Scan() {
								line := scanner.Text()
								if strings.Contains(line, "url=https://") {
									parts := strings.Split(line, "url=")
									if len(parts) > 1 {
										urlVal := strings.Fields(parts[1])[0]
										urlChan <- urlVal
										return
									}
								}
							}
						})
					} else {
						safe.Go(func() {
							scanner := bufio.NewScanner(stderrPipe)
							for scanner.Scan() {
								line := scanner.Text()
								if strings.Contains(line, "trycloudflare.com") {
									fields := strings.Fields(line)
									for _, f := range fields {
										if strings.Contains(f, "trycloudflare.com") {
											urlVal := strings.TrimSpace(f)
											urlVal = strings.Trim(urlVal, "| \t\r\n")
											if strings.HasPrefix(urlVal, "https://") {
												urlChan <- urlVal
												return
											}
										}
									}
								}
							}
						})
					}

					safe.Go(func() {
						var publicURL string
						if project.TunnelType == "ngrok" {
							select {
							case urlVal := <-urlChan:
								publicURL = urlVal
							case <-time.After(5 * time.Second):
								val, err := pollNgrokAPI()
								if err == nil && val != "" {
									publicURL = val
								}
							}
						} else {
							select {
							case urlVal := <-urlChan:
								publicURL = urlVal
							case <-time.After(15 * time.Second):
								// Timeout
							}
						}

						if publicURL != "" {
							pm.mu.Lock()
							proc.PublicURL = publicURL
							consecutiveTunnelRestarts = 0 // Reset on success
							pm.mu.Unlock()
							fmt.Printf("[watchdog] Tunnel restarted successfully. New URL: %s\n", publicURL)
						} else {
							fmt.Printf("[watchdog] Tunnel restarted but failed to get public URL\n")
						}
					})
				}
			}

			pm.mu.Unlock()
		}
	}
}

func getBackoffDelay(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures >= 5 {
		return 40 * time.Second
	}
	delays := []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second}
	return delays[failures]
}

func isPortOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func FreePort(port int) {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd.exe", "/c", fmt.Sprintf("netstat -ano | findstr :%d", port))
		output, err := cmd.Output()
		if err != nil {
			return
		}
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				localAddr := fields[1]
				pid := fields[len(fields)-1]
				pid = strings.TrimSpace(pid)
				if strings.HasSuffix(localAddr, fmt.Sprintf(":%d", port)) && pid != "0" && pid != "" {
					fmt.Printf("[startup] Port %d is occupied by PID %s. Killing it...\n", port, pid)
					_ = exec.Command("taskkill", "/F", "/PID", pid).Run()
				}
			}
		}
	} else {
		cmd := exec.Command("sh", "-c", fmt.Sprintf("lsof -t -i:%d", port))
		output, err := cmd.Output()
		if err == nil {
			pids := strings.Fields(string(output))
			for _, pid := range pids {
				_ = exec.Command("kill", "-9", pid).Run()
			}
		}
	}
}

func killProcessTree(proc *os.Process) {
	if proc == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(proc.Pid)).Run()
	} else {
		_ = proc.Kill()
	}
}

