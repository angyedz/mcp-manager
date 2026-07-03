package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServeHTTP_Auth(t *testing.T) {
	adminToken := "test-token-123"
	router := NewMCPRouter(adminToken, "http://127.0.0.1:3000")

	// Case 1: Missing token (should return 401)
	req := httptest.NewRequest("GET", "/sse", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rr.Code)
	}

	// Case 2: Invalid Bearer token (should return 401)
	req = httptest.NewRequest("GET", "/sse", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rr.Code)
	}

	// Case 3: Valid Bearer token
	// Without the upstream server on 3000, ServeHTTP for /sse should try to forward
	// and fail with 502 Bad Gateway (upstream SSE unavailable) or proxy bad gateway, but not 401.
	req = httptest.NewRequest("GET", "/sse", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Errorf("Expected status 502 (Bad Gateway), got %d", rr.Code)
	}

	// Case 4: Valid query parameter token
	req = httptest.NewRequest("GET", "/sse?token="+adminToken, nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Errorf("Expected status 502 (Bad Gateway), got %d", rr.Code)
	}
}

func TestSSEKeepalive_Rewrite(t *testing.T) {
	adminToken := "test-token-123"

	// Create upstream server on 127.0.0.1:3000
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		fmt.Fprint(w, "event: endpoint\n")
		fmt.Fprint(w, "data: /message?session_id=abcdef123456\n\n")
	})

	server := &http.Server{
		Addr:    "127.0.0.1:3000",
		Handler: mux,
	}

	go func() {
		_ = server.ListenAndServe()
	}()
	defer server.Close()

	// Wait a moment for server to start
	time.Sleep(100 * time.Millisecond)

	router := NewMCPRouter(adminToken, "http://127.0.0.1:3000")

	req := httptest.NewRequest("GET", "/sse?token="+adminToken, nil)
	rr := httptest.NewRecorder()

	// Call ServeHTTP
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	expected := "data: /message?session_id=abcdef123456&token=test-token-123\n"
	if !strings.Contains(body, expected) {
		t.Errorf("Expected body to contain modified endpoint line %q, got: %q", expected, body)
	}
}

func TestRemoteURLRouting(t *testing.T) {
	adminToken := "test-token-123"
	targetURL := "https://mcp.website.com/mcp"
	router := NewMCPRouter(adminToken, targetURL)

	// Test 1: getUpstreamURL logic
	req := httptest.NewRequest("GET", "/sse?token="+adminToken+"&session=abc", nil)
	resolved := router.getUpstreamURL(req)
	expectedURL := "https://mcp.website.com/mcp?session=abc"
	if resolved != expectedURL {
		t.Errorf("Expected upstream URL %q, got %q", expectedURL, resolved)
	}

	// Test 2: Director rewrites request Host and path duplication
	proxyReq := httptest.NewRequest("POST", "/mcp/session/123", nil)
	router.proxy.Director(proxyReq)

	if proxyReq.Host != "mcp.website.com" {
		t.Errorf("Expected Host header %q, got %q", "mcp.website.com", proxyReq.Host)
	}
	if proxyReq.URL.Path != "/mcp/session/123" {
		t.Errorf("Expected Path %q, got %q", "/mcp/session/123", proxyReq.URL.Path)
	}
}
