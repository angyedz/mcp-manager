package cloudflare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"mcp-manager/internal/config"
)

const WorkerScript = `
export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);

    if (url.pathname === "/admin/update") {
      const newUrl = await request.text();
      if (!newUrl) {
        return new Response("Empty tunnel URL", { status: 400 });
      }
      await env.MCP_KV.put("TUNNEL_URL", newUrl);
      return new Response("Updated tunnel URL to: " + newUrl, { status: 200 });
    }

    const tunnelUrl = await env.MCP_KV.get("TUNNEL_URL");
    if (!tunnelUrl) {
      return new Response("Tunnel URL not configured in KV (MCP_KV)", { status: 502 });
    }

    // Rewrite request target
    const targetUrl = new URL(url.pathname + url.search, tunnelUrl);

    // Forward request
    try {
      const headers = new Headers(request.headers);
      headers.delete("host");
      headers.set("ngrok-skip-browser-warning", "true");
      const isSSE = url.pathname === "/sse" || headers.get("Accept") === "text/event-stream";

      const proxyResp = await fetch(targetUrl.toString(), {
        method: request.method,
        headers: headers,
        body: request.method !== "GET" && request.method !== "HEAD" ? request.body : undefined,
        redirect: "manual"
      });

      if (isSSE) {
        // SSE requires streaming response
        const { readable, writable } = new TransformStream();
        proxyResp.body.pipeTo(writable);
        return new Response(readable, proxyResp);
      }

      return proxyResp;
    } catch (e) {
      return new Response("Failed to proxy request: " + e.message, { status: 502 });
    }
  }
};
`

func getAccountID(token string) (string, error) {
	req, err := http.NewRequest("GET", "https://api.cloudflare.com/client/v4/accounts", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get accounts, status: %s, body: %s", resp.Status, body)
	}

	var res struct {
		Result []struct {
			ID string `json:"id"`
		} `json:"result"`
		Success bool `json:"success"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	if !res.Success || len(res.Result) == 0 {
		return "", fmt.Errorf("no accounts found or request failed")
	}

	return res.Result[0].ID, nil
}

func getOrCreateKVNamespace(token, accountID, title string) (string, error) {
	listURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/storage/kv/namespaces", accountID)
	req, err := http.NewRequest("GET", listURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to list KV namespaces, status: %s, body: %s", resp.Status, body)
	}

	var listRes struct {
		Result []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"result"`
		Success bool `json:"success"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&listRes); err != nil {
		return "", err
	}

	for _, ns := range listRes.Result {
		if ns.Title == title {
			return ns.ID, nil
		}
	}

	// Create namespace if not found
	createBody, _ := json.Marshal(map[string]string{"title": title})
	createReq, err := http.NewRequest("POST", listURL, bytes.NewReader(createBody))
	if err != nil {
		return "", err
	}
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set("Content-Type", "application/json")

	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		return "", err
	}
	defer createResp.Body.Close()

	if createResp.StatusCode != http.StatusOK && createResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createResp.Body)
		return "", fmt.Errorf("failed to create KV namespace, status: %s, body: %s", createResp.Status, body)
	}

	var createRes struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
		Success bool `json:"success"`
	}

	if err := json.NewDecoder(createResp.Body).Decode(&createRes); err != nil {
		return "", err
	}

	if !createRes.Success {
		return "", fmt.Errorf("failed to create KV namespace")
	}

	return createRes.Result.ID, nil
}

// UpdateKV updates a key in the CF KV namespace
func UpdateKV(key, value string) error {
	token, err := config.GetSecret("cf_api_token")
	if err != nil || token == "" {
		return fmt.Errorf("cloudflare API token not configured")
	}

	accountID, err := getAccountID(token)
	if err != nil {
		return fmt.Errorf("failed to resolve Cloudflare account ID: %w", err)
	}

	nsID, err := getOrCreateKVNamespace(token, accountID, "MCP_CONFIG")
	if err != nil {
		return fmt.Errorf("failed to find/create KV namespace 'MCP_CONFIG': %w", err)
	}

	writeURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/storage/kv/namespaces/%s/values/%s", accountID, nsID, key)
	req, err := http.NewRequest("PUT", writeURL, strings.NewReader(value))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to write value to KV store, status: %s, body: %s", resp.Status, body)
	}

	return nil
}

// DeployWorker uploads the worker script and binds the KV namespace to it
func DeployWorker(script, name string) error {
	token, err := config.GetSecret("cf_api_token")
	if err != nil || token == "" {
		return fmt.Errorf("cloudflare API token not configured")
	}

	accountID, err := getAccountID(token)
	if err != nil {
		return fmt.Errorf("failed to resolve Cloudflare account ID: %w", err)
	}

	nsID, err := getOrCreateKVNamespace(token, accountID, "MCP_CONFIG")
	if err != nil {
		return fmt.Errorf("failed to find/create KV namespace 'MCP_CONFIG': %w", err)
	}

	// Prepare multipart body
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// 1. Metadata specifying main module and KV bindings
	metadata := fmt.Sprintf(`{
		"main_module": "worker.js",
		"compatibility_date": "2024-05-02",
		"compatibility_flags": ["global_fetch_strictly_public"],
		"bindings": [
			{
				"type": "kv_namespace",
				"name": "MCP_KV",
				"namespace_id": "%s"
			}
		]
	}`, nsID)

	metadataHeader := make(textproto.MIMEHeader)
	metadataHeader.Set("Content-Disposition", `form-data; name="metadata"`)
	metadataHeader.Set("Content-Type", "application/json")
	metadataPart, err := writer.CreatePart(metadataHeader)
	if err != nil {
		return err
	}
	_, _ = metadataPart.Write([]byte(metadata))

	// 2. Script source file
	scriptHeader := make(textproto.MIMEHeader)
	scriptHeader.Set("Content-Disposition", `form-data; name="script"; filename="worker.js"`)
	scriptHeader.Set("Content-Type", "application/javascript+module")
	scriptPart, err := writer.CreatePart(scriptHeader)
	if err != nil {
		return err
	}
	_, _ = scriptPart.Write([]byte(script))

	_ = writer.Close()

	deployURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/workers/scripts/%s", accountID, name)
	req, err := http.NewRequest("PUT", deployURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to deploy worker script, status: %s, body: %s", resp.Status, respBody)
	}

	return nil
}

// GetWorkerURL queries the workers.dev subdomain of the account and returns the worker URL
func GetWorkerURL(workerName string) (string, error) {
	token, err := config.GetSecret("cf_api_token")
	if err != nil || token == "" {
		return "", fmt.Errorf("cloudflare API token not configured")
	}

	accountID, err := getAccountID(token)
	if err != nil {
		return "", fmt.Errorf("failed to resolve Cloudflare account ID: %w", err)
	}

	subdomainURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/workers/subdomain", accountID)
	req, err := http.NewRequest("GET", subdomainURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get workers subdomain, status: %s, body: %s", resp.Status, body)
	}

	var res struct {
		Result struct {
			Subdomain string `json:"subdomain"`
		} `json:"result"`
		Success bool `json:"success"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	if !res.Success || res.Result.Subdomain == "" {
		return "", fmt.Errorf("failed to resolve subdomain from Cloudflare response")
	}

	return fmt.Sprintf("https://%s.%s.workers.dev", workerName, res.Result.Subdomain), nil
}
