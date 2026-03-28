package integration

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	linearClientID = "9bf3045f8304d304e149599d11471426"
	linearAuthURL  = "https://linear.app/oauth/authorize"
	linearTokenURL = "https://api.linear.app/oauth/token"
	linearScopes   = "read,write,issues:create"
)

// ConnectLinear runs the interactive Linear connection flow:
// OAuth2 PKCE, team/project selection, webhook setup, credential storage.
func ConnectLinear() error {
	// Ensure store is open before config operations
	if _, err := StoreEnsure(); err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	// Check if already connected
	existingTeam, _ := StoreGetConfig("linear.team-key")
	if existingTeam != "" {
		fmt.Printf("  Linear is already connected (team: %s).\n", existingTeam)
		fmt.Print("  Reconnect? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		if !strings.HasPrefix(strings.TrimSpace(strings.ToLower(answer)), "y") {
			return nil
		}
	}

	// Step 1: OAuth2 PKCE flow
	fmt.Println()
	fmt.Println("  Opening Linear authorization in your browser...")

	token, err := OAuthPKCE()
	if err != nil {
		return fmt.Errorf("OAuth flow failed: %w", err)
	}

	fmt.Println("  \u2713 Authenticated with Linear")
	fmt.Println()

	// Step 2: Fetch teams and pick one
	teams, err := FetchLinearTeams(token)
	if err != nil {
		return fmt.Errorf("fetch teams: %w", err)
	}
	if len(teams) == 0 {
		return fmt.Errorf("no teams found in your Linear workspace")
	}

	team := teams[0]
	if len(teams) > 1 {
		fmt.Println("  Select a team:")
		for i, t := range teams {
			fmt.Printf("    %d. %s (%s)\n", i+1, t.Name, t.Key)
		}
		fmt.Print("  > ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		idx, err := strconv.Atoi(strings.TrimSpace(input))
		if err != nil || idx < 1 || idx > len(teams) {
			return fmt.Errorf("invalid selection")
		}
		team = teams[idx-1]
	} else {
		fmt.Printf("  Team: %s (%s)\n", team.Name, team.Key)
	}
	fmt.Println()

	// Step 3: Optionally pick a project
	projects, _ := FetchLinearProjects(token, team.ID)
	var projectID string
	if len(projects) > 0 {
		fmt.Println("  Select a project (optional, enter to skip):")
		for i, p := range projects {
			fmt.Printf("    %d. %s\n", i+1, p.Name)
		}
		fmt.Print("  > ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input != "" {
			idx, err := strconv.Atoi(input)
			if err == nil && idx >= 1 && idx <= len(projects) {
				projectID = projects[idx-1].ID
				fmt.Printf("  Project: %s\n", projects[idx-1].Name)
			}
		}
		fmt.Println()
	}

	// Step 4: Webhook setup (optional)
	fmt.Println("  Set up webhook? Linear events will flow back to Spire.")
	fmt.Print("  Webhook URL (enter to skip): ")
	reader := bufio.NewReader(os.Stdin)
	webhookURL, _ := reader.ReadString('\n')
	webhookURL = strings.TrimSpace(webhookURL)

	var webhookSecret string
	if webhookURL != "" {
		// Ensure URL ends with /webhook
		if !strings.HasSuffix(webhookURL, "/webhook") {
			webhookURL = strings.TrimSuffix(webhookURL, "/") + "/webhook"
		}

		secret, err := CreateLinearWebhook(token, team.ID, webhookURL)
		if err != nil {
			fmt.Printf("  \u26a0 Webhook creation failed: %s\n", err)
			fmt.Println("  (You can set it up manually in Linear settings)")
		} else {
			webhookSecret = secret
			fmt.Printf("  \u2713 Webhook created \u2192 %s\n", webhookURL)
		}
		fmt.Println()
	}

	// Step 5: Store credentials
	// Token -> keychain (secret, per-machine)
	if err := KeychainSet("linear.access-token", token); err != nil {
		// Fallback: warn but don't fail
		fmt.Printf("  \u26a0 Could not save token to keychain: %s\n", err)
		fmt.Println("  Set LINEAR_API_KEY env var instead.")
	}

	if webhookSecret != "" {
		if err := KeychainSet("linear.webhook-secret", webhookSecret); err != nil {
			fmt.Printf("  \u26a0 Could not save webhook secret to keychain: %s\n", err)
			fmt.Printf("  Set LINEAR_WEBHOOK_SECRET=%s in your webhook deployment.\n", webhookSecret)
		}
	}

	// Non-secret config -> store config (shared via Dolt)
	StoreSetConfig("linear.team-id", team.ID)
	StoreSetConfig("linear.team-key", team.Key)
	if projectID != "" {
		StoreSetConfig("linear.project-id", projectID)
	}
	if webhookURL != "" {
		StoreSetConfig("linear.webhook-url", webhookURL)
	}

	fmt.Println()
	fmt.Println("  \u2713 Linear connected")
	fmt.Printf("    Team: %s (%s)\n", team.Name, team.Key)
	if webhookURL != "" {
		fmt.Printf("    Webhook: %s\n", webhookURL)
	}
	fmt.Println("    Credentials saved to system keychain")
	fmt.Println()
	fmt.Println("  Epics will sync automatically via the daemon.")

	return nil
}

// DisconnectLinear removes all Linear credentials and config.
func DisconnectLinear() error {
	// Ensure store is open before config operations
	if _, err := StoreEnsure(); err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	// Remove keychain entries
	KeychainDelete("linear.access-token")
	KeychainDelete("linear.webhook-secret")
	fmt.Println("  \u2713 Token removed from keychain")

	// Remove store config entries
	StoreDeleteConfig("linear.team-id")
	StoreDeleteConfig("linear.team-key")
	StoreDeleteConfig("linear.project-id")
	StoreDeleteConfig("linear.webhook-url")
	fmt.Println("  \u2713 Config removed from beads")

	fmt.Println()
	fmt.Println("  Linear disconnected. Epic sync and webhooks are disabled.")

	return nil
}

// --- OAuth2 PKCE flow ---

// OAuthPKCE runs the OAuth2 PKCE authorization flow and returns the access token.
func OAuthPKCE() (string, error) {
	// Generate PKCE code verifier + challenge
	verifierBytes := make([]byte, 32)
	rand.Read(verifierBytes)
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	challengeHash := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])

	// Random state
	stateBytes := make([]byte, 16)
	rand.Read(stateBytes)
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	// Start local callback server
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return "", fmt.Errorf("start callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	redirectURI := fmt.Sprintf("http://localhost:%d/callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errCh <- fmt.Errorf("state mismatch")
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			errCh <- fmt.Errorf("OAuth error: %s \u2014 %s", errParam, r.URL.Query().Get("error_description"))
			fmt.Fprintf(w, "<html><body><h2>Authorization failed</h2><p>%s</p><p>You can close this tab.</p></body></html>", errParam)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("no code in callback")
			http.Error(w, "Missing code", http.StatusBadRequest)
			return
		}
		codeCh <- code
		fmt.Fprint(w, "<html><body><h2>\u2713 Spire authorized</h2><p>You can close this tab and return to your terminal.</p></body></html>")
	})

	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	// Build authorization URL
	authParams := url.Values{
		"client_id":             {linearClientID},
		"response_type":        {"code"},
		"redirect_uri":         {redirectURI},
		"scope":                {linearScopes},
		"code_challenge":       {codeChallenge},
		"code_challenge_method": {"S256"},
		"state":                {state},
		"prompt":               {"consent"},
	}
	authURL := linearAuthURL + "?" + authParams.Encode()

	// Open browser
	OpenBrowser(authURL)

	fmt.Printf("  Waiting for authorization on localhost:%d...\n", port)

	// Wait for callback (timeout after 5 minutes)
	select {
	case code := <-codeCh:
		return ExchangeCode(code, codeVerifier, redirectURI)
	case err := <-errCh:
		return "", err
	case <-time.After(5 * time.Minute):
		return "", fmt.Errorf("authorization timed out (5 minutes)")
	}
}

// ExchangeCode exchanges an authorization code for an access token.
func ExchangeCode(code, codeVerifier, redirectURI string) (string, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {linearClientID},
		"redirect_uri":  {redirectURI},
		"code":          {code},
		"code_verifier": {codeVerifier},
	}

	resp, err := http.PostForm(linearTokenURL, data)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}

	if result.AccessToken == "" {
		return "", fmt.Errorf("empty access token in response")
	}

	return result.AccessToken, nil
}

// OpenBrowser opens a URL in the system's default browser.
func OpenBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		log.Printf("Open this URL in your browser:\n  %s", url)
		return
	}
	cmd.Run()
}

// --- Linear GraphQL helpers for connect flow ---

// FetchLinearTeams fetches all teams from the authenticated user's workspace.
func FetchLinearTeams(token string) ([]LinearTeam, error) {
	data, err := LinearGraphQL(token, `query { teams { nodes { id name key } } }`, nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Teams struct {
			Nodes []LinearTeam `json:"nodes"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Teams.Nodes, nil
}

// FetchLinearProjects fetches projects for a team.
func FetchLinearProjects(token, teamID string) ([]LinearProject, error) {
	data, err := LinearGraphQL(token, `
		query($teamId: String!) {
			team(id: $teamId) {
				projects { nodes { id name } }
			}
		}
	`, map[string]any{"teamId": teamID})
	if err != nil {
		return nil, err
	}
	var result struct {
		Team struct {
			Projects struct {
				Nodes []LinearProject `json:"nodes"`
			} `json:"projects"`
		} `json:"team"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Team.Projects.Nodes, nil
}

// CreateLinearWebhook creates a webhook subscription for a team.
func CreateLinearWebhook(token, teamID, webhookURL string) (string, error) {
	data, err := LinearGraphQL(token, `
		mutation($input: WebhookCreateInput!) {
			webhookCreate(input: $input) {
				success
				webhook { id secret }
			}
		}
	`, map[string]any{
		"input": map[string]any{
			"url":           webhookURL,
			"teamId":        teamID,
			"resourceTypes": []string{"Issue", "Comment", "Project"},
			"enabled":       true,
			"label":         "Spire",
		},
	})
	if err != nil {
		return "", err
	}
	var result struct {
		WebhookCreate struct {
			Success bool `json:"success"`
			Webhook struct {
				ID     string `json:"id"`
				Secret string `json:"secret"`
			} `json:"webhook"`
		} `json:"webhookCreate"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	if !result.WebhookCreate.Success {
		return "", fmt.Errorf("webhook creation failed")
	}
	return result.WebhookCreate.Webhook.Secret, nil
}
