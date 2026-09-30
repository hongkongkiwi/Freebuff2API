// Command freebuff-token performs the Codebuff OAuth device-code login and
// prints (or stores) the resulting authToken so it can be pasted into
// AUTH_TOKENS.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultBaseURL = "https://www.codebuff.com"

func randRead(buf []byte) (int, error) {
	return rand.Read(buf)
}

type cliCodeResponse struct {
	LoginURL        string `json:"loginUrl"`
	FingerprintHash string `json:"fingerprintHash"`
	ExpiresAt       string `json:"expiresAt"`
}

type cliStatusResponse struct {
	User *struct {
		AuthToken string `json:"authToken"`
		Email     string `json:"email"`
		ID        string `json:"id"`
	} `json:"user"`
}

func main() {
	baseURL := flag.String("base-url", defaultBaseURL, "upstream base URL")
	timeout := flag.Duration("timeout", 5*time.Minute, "how long to poll for authorization")
	writeConfig := flag.Bool("write-config", false, "append the token to config.json AUTH_TOKENS when login succeeds")
	configPath := flag.String("config", "config.json", "config file used with -write-config")
	fingerprint := flag.String("fingerprint", "", "device fingerprint id (default: codebuff-cli-<random>)")
	flag.Parse()

	if *fingerprint == "" {
		*fingerprint = generateFingerprint()
	}

	client := &http.Client{Timeout: 20 * time.Second}

	fmt.Printf("Requesting login URL (fingerprint %s)...\n", *fingerprint)
	codeResp, err := postCliCode(client, *baseURL, *fingerprint)
	if err != nil {
		fatalf("request login URL: %v", err)
	}

	fmt.Println()
	fmt.Println("1. Open this URL in a browser and sign in:")
	fmt.Println("   " + codeResp.LoginURL)
	fmt.Println("2. Authorize the CLI with your Google account.")
	fmt.Printf("3. Waiting for authorization (up to %s)...\n", *timeout)

	authToken, email := pollForToken(client, *baseURL, *fingerprint, codeResp, *timeout)

	fmt.Println()
	fmt.Printf("Login succeeded for %s\n", email)
	fmt.Println()
	fmt.Println("AUTH_TOKEN:")
	fmt.Println(authToken)

	if *writeConfig {
		if err := appendTokenToConfig(*configPath, authToken); err != nil {
			fatalf("write config: %v", err)
		}
		fmt.Printf("\nToken appended to %s\n", *configPath)
	}
}

func generateFingerprint() string {
	buf := make([]byte, 6)
	if _, err := randRead(buf); err != nil {
		fatalf("generate fingerprint: %v", err)
	}
	encoded := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(buf)
	if len(encoded) > 8 {
		encoded = encoded[:8]
	}
	return "codebuff-cli-" + encoded
}

func postCliCode(client *http.Client, baseURL, fingerprint string) (*cliCodeResponse, error) {
	body, _ := json.Marshal(map[string]string{"fingerprintId": fingerprint})
	requestURL, err := url.JoinPath(baseURL, "/api/auth/cli/code")
	if err != nil {
		return nil, err
	}
	resp, err := client.Post(requestURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var parsed cliCodeResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if parsed.LoginURL == "" || parsed.FingerprintHash == "" {
		return nil, fmt.Errorf("response missing loginUrl/fingerprintHash: %s", strings.TrimSpace(string(data)))
	}
	return &parsed, nil
}

func pollForToken(client *http.Client, baseURL, fingerprint string, code *cliCodeResponse, timeout time.Duration) (string, string) {
	requestURL, err := url.JoinPath(baseURL, "/api/auth/cli/status")
	if err != nil {
		fatalf("build status URL: %v", err)
	}

	deadline := time.Now().Add(timeout)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		statusURL := fmt.Sprintf("%s?fingerprintId=%s&fingerprintHash=%s&expiresAt=%s",
			requestURL,
			url.QueryEscape(fingerprint),
			url.QueryEscape(code.FingerprintHash),
			url.QueryEscape(code.ExpiresAt),
		)
		resp, err := client.Get(statusURL)
		if err == nil {
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				var parsed cliStatusResponse
				if json.Unmarshal(data, &parsed) == nil && parsed.User != nil && parsed.User.AuthToken != "" {
					return parsed.User.AuthToken, parsed.User.Email
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	fatalf("timed out waiting for authorization")
	return "", ""
}

// appendTokenToConfig adds the token to the AUTH_TOKENS array of a JSON config,
// creating the file with defaults when it does not exist yet.
func appendTokenToConfig(path, token string) error {
	var config map[string]any
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if os.IsNotExist(err) {
		config = map[string]any{
			"LISTEN_ADDR":       ":8080",
			"UPSTREAM_BASE_URL": defaultBaseURL,
		}
	} else {
		return err
	}

	tokens := []string{}
	if raw, ok := config["AUTH_TOKENS"].([]any); ok {
		for _, entry := range raw {
			if text, ok := entry.(string); ok && strings.TrimSpace(text) != "" {
				tokens = append(tokens, text)
			}
		}
	}
	for _, existing := range tokens {
		if existing == token {
			fmt.Println("Token already present in config.")
			return nil
		}
	}
	config["AUTH_TOKENS"] = append(tokens, token)

	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "freebuff-token: "+format+"\n", args...)
	os.Exit(1)
}
