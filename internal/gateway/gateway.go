package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"mcp-manager/internal/audit"
	"mcp-manager/internal/safe"
)

type MCPRouter struct {
	AdminToken string
	TargetURL  string
	proxy      *httputil.ReverseProxy
}

func NewMCPRouter(adminToken string, targetURL string) *MCPRouter {
	target, _ := url.Parse(targetURL)
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Wrap original Director to set Host (essential for TLS SNI) and prevent path duplication.
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host

		targetPath := strings.TrimSuffix(target.Path, "/")
		if targetPath != "" {
			doublePrefix := targetPath + targetPath
			if strings.HasPrefix(req.URL.Path, doublePrefix) {
				req.URL.Path = strings.TrimPrefix(req.URL.Path, targetPath)
			}
		}
	}

	// Optimize Transport connection pool, add a connection retry dialer, and disable response timeout for SSE streams
	proxy.Transport = &http.Transport{
		ResponseHeaderTimeout: 0,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var conn net.Conn
			var err error
			for i := 0; i < 3; i++ {
				dialer := &net.Dialer{
					Timeout: 2 * time.Second,
				}
				conn, err = dialer.DialContext(ctx, network, addr)
				if err == nil {
					return conn, nil
				}
				// Retry connection after a short delay (e.g. if proxy is restarting)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
			}
			return nil, err
		},
	}

	// Add error handler for robust error reporting
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		payload, marErr := json.Marshal(map[string]string{"error": err.Error()})
		if marErr == nil {
			audit.LogEntry("error", "proxy_error", payload)
		} else {
			audit.LogEntry("error", "proxy_error", []byte(`{"error":"failed to marshal proxy error"}`))
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("Failed to connect to upstream MCP server"))
	}

	// Intercept response to log JSON-RPC response payloads
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == http.StatusOK && resp.Request != nil && resp.Request.Method == http.MethodPost {
			bodyBytes, err := io.ReadAll(resp.Body)
			if err == nil {
				resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

				// Extract method name from original request context
				method, _ := resp.Request.Context().Value(ctxKeyMethod).(string)
				audit.LogEntry("response", method, bodyBytes)
			}
		}
		return nil
	}

	return &MCPRouter{
		AdminToken: adminToken,
		TargetURL:  targetURL,
		proxy:      proxy,
	}
}

type contextKey string

const ctxKeyMethod contextKey = "mcpMethod"

// DiscoverTools is kept as a no-op for compatibility
func (r *MCPRouter) DiscoverTools() error {
	return nil
}

// getUpstreamURL resolves the final upstream SSE URL, copying query parameters from request (excluding token)
func (router *MCPRouter) getUpstreamURL(r *http.Request) string {
	target, err := url.Parse(router.TargetURL)
	if err != nil {
		return router.TargetURL
	}

	var upstream *url.URL
	if target.Path != "" && target.Path != "/" {
		uCopy := *target
		upstream = &uCopy
	} else {
		upstream = target.ResolveReference(&url.URL{Path: "/sse"})
	}

	q := r.URL.Query()
	q.Del("token")

	targetQ := target.Query()
	for k, vs := range targetQ {
		for _, v := range vs {
			q.Add(k, v)
		}
	}

	upstream.RawQuery = q.Encode()
	return upstream.String()
}

// handleSSEKeepalive proxies the SSE connection and sends a comment ping every 15s
// to prevent ngrok / reverse proxies from closing idle connections.
func (router *MCPRouter) handleSSEKeepalive(w http.ResponseWriter, r *http.Request, token string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Fallback: just proxy normally
		router.proxy.ServeHTTP(w, r)
		return
	}

	// Open connection to upstream SSE
	upstreamURL := router.getUpstreamURL(r)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}
	// Copy headers from client request
	for k, vals := range r.Header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	client := &http.Client{Timeout: 0} // No timeout for SSE
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "upstream SSE unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy upstream headers to client
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	var mu sync.Mutex
	writeAndFlush := func(data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if _, err := w.Write(data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// Start heartbeat goroutine — sends SSE comment every 15s
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Close upstream response body on client disconnect to unblock streaming loop
	safe.Go(func() {
		<-ctx.Done()
		resp.Body.Close()
	})

	safe.Go(func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = writeAndFlush([]byte(": ping\n\n"))
			}
		}
	})

	// Stream upstream SSE data to client and rewrite the endpoint URI if token is provided
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			modifiedLine := line
			if token != "" && strings.HasPrefix(line, "data: ") {
				trimmed := strings.TrimRight(line, "\r\n")
				suffix := line[len(trimmed):]
				uri := strings.TrimPrefix(trimmed, "data: ")

				// Handle absolute URLs by converting them to relative path to route through our gateway
				if u, err := url.Parse(uri); err == nil {
					if u.Scheme == "http" || u.Scheme == "https" {
						pathAndQuery := u.Path
						if u.RawQuery != "" {
							pathAndQuery += "?" + u.RawQuery
						}
						uri = pathAndQuery
					}
				}

				if strings.HasPrefix(uri, "/") {
					if strings.Contains(uri, "?") {
						uri += "&token=" + token
					} else {
						uri += "?token=" + token
					}
					modifiedLine = "data: " + uri + suffix
				}
			}
			if werr := writeAndFlush([]byte(modifiedLine)); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (router *MCPRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Enable CORS for all incoming requests (crucial for browser-based clients like Notion)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept")

	// Handle CORS preflight (OPTIONS)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 1. Bearer Token check (either via Authorization header or query parameter "token")
	token := r.URL.Query().Get("token")
	if token == "" {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
				token = authHeader[7:]
			} else {
				token = authHeader
			}
		}
	}

	if token != router.AdminToken {
		http.Error(w, "Unauthorized: Invalid or missing token", http.StatusUnauthorized)
		return
	}

	// 2. SSE endpoint — use keepalive handler
	if r.URL.Path == "/sse" || strings.HasSuffix(r.URL.Path, "/sse") {
		router.handleSSEKeepalive(w, r, token)
		return
	}

	// 3. Intercept and log request body for POST requests
	if r.Method == http.MethodPost {
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			var rpcReq struct {
				Method string `json:"method"`
			}
			var method string
			if err := json.Unmarshal(bodyBytes, &rpcReq); err == nil {
				method = rpcReq.Method
			}
			audit.LogEntry("request", method, bodyBytes)

			// Store the method in context so ModifyResponse can log it
			ctx := context.WithValue(r.Context(), ctxKeyMethod, method)
			r = r.WithContext(ctx)
		}
	}

	// 4. Proxy request to port 3000
	router.proxy.ServeHTTP(w, r)
}



