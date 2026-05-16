package tunnel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	ngrokAPIURL    = "http://127.0.0.1:4040/api"
	startupTimeout = 15 * time.Second
)

// NgrokTunnel manages an ngrok tunnel process.
type NgrokTunnel struct {
	targetURL string
	ngrokURL  string
	apiURL    string
	cmd       string
	cmdObj    *exec.Cmd
}

// NewNgrokTunnel creates a new ngrok tunnel manager.
func NewNgrokTunnel(targetURL string, ngrokURL string) *NgrokTunnel {
	return &NgrokTunnel{
		targetURL: targetURL,
		ngrokURL:  ngrokURL,
		apiURL:    ngrokAPIURL,
		cmd:       "ngrok",
	}
}

// LocalTunnelTarget builds the local tunnel target URL from host and port.
func LocalTunnelTarget(host string, port int) string {
	localHost := strings.TrimSpace(host)
	if localHost == "" {
		localHost = "127.0.0.1"
	}
	if localHost == "0.0.0.0" || localHost == "::" {
		localHost = "127.0.0.1"
	}
	if strings.Contains(localHost, ":") && !strings.HasPrefix(localHost, "[") {
		localHost = fmt.Sprintf("[%s]", localHost)
	}
	return fmt.Sprintf("http://%s:%d", localHost, port)
}

// Start starts the ngrok tunnel and returns the public URL.
func (nt *NgrokTunnel) Start() (string, error) {
	// Check if ngrok is available
	if _, err := exec.LookPath(nt.cmd); err != nil {
		return "", fmt.Errorf(
			"ngrok is not installed or is not on PATH. Install it, then run " +
				"`ngrok config add-authtoken <token>` once.")
	}

	args := []string{
		"http",
		"--log=stderr",
		"--log-level=error",
		nt.targetURL,
	}
	if nt.ngrokURL != "" {
		args = append(args, fmt.Sprintf("--url=%s", nt.ngrokURL))
	}

	cmd := exec.Command(nt.cmd, args...)
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("cannot start ngrok: %w", err)
	}
	nt.cmdObj = cmd

	publicURL, err := nt.waitForPublicURL()
	if err != nil {
		nt.Stop()
		return "", err
	}

	return publicURL, nil
}

// Stop terminates the ngrok tunnel.
func (nt *NgrokTunnel) Stop() {
	if nt.cmdObj == nil || nt.cmdObj.Process == nil {
		return
	}
	if err := nt.cmdObj.Process.Signal(os.Interrupt); err != nil {
		nt.cmdObj.Process.Kill()
	}

	done := make(chan struct{}, 1)
	go func() {
		nt.cmdObj.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		nt.cmdObj.Process.Kill()
	}
	nt.cmdObj = nil
}

func (nt *NgrokTunnel) waitForPublicURL() (string, error) {
	deadline := time.Now().Add(startupTimeout)
	lastError := "ngrok did not report a public URL"

	for time.Now().Before(deadline) {
		// Check if process exited prematurely
		if nt.cmdObj != nil && nt.cmdObj.ProcessState != nil && nt.cmdObj.ProcessState.Exited() {
			return "", fmt.Errorf("ngrok exited before creating a tunnel")
		}

		for _, apiURL := range ngrokAgentURLs(nt.apiURL) {
			publicURL, err := fetchNgrokPublicURL(apiURL)
			if err != nil {
				lastError = err.Error()
				continue
			}
			if publicURL != "" {
				return publicURL, nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	return "", fmt.Errorf("timed out waiting for ngrok tunnel: %s", lastError)
}

func ngrokAgentURLs(apiURL string) []string {
	normalized := strings.TrimRight(apiURL, "/")
	if strings.HasSuffix(normalized, "/endpoints") || strings.HasSuffix(normalized, "/tunnels") {
		return []string{normalized}
	}
	return []string{
		normalized + "/endpoints",
		normalized + "/tunnels",
	}
}

func fetchNgrokPublicURL(apiURL string) (string, error) {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}

	return parseNgrokPublicURL(payload), nil
}

func parseNgrokPublicURL(payload map[string]any) string {
	// Try endpoints first (ngrok v3), then tunnels
	records, _ := payload["endpoints"].([]any)
	if records == nil {
		records, _ = payload["tunnels"].([]any)
	}

	var httpsURL, httpURL string
	for _, raw := range records {
		record, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		url, _ := record["url"].(string)
		if url == "" {
			url, _ = record["public_url"].(string)
		}
		if strings.HasPrefix(url, "https://") && httpsURL == "" {
			httpsURL = url
		}
		if strings.HasPrefix(url, "http://") && httpURL == "" {
			httpURL = url
		}
	}

	if httpsURL != "" {
		return httpsURL
	}
	return httpURL
}
