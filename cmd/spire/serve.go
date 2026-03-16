package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"crypto/rand"
)

func cmdServe(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	port := "8080"

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 >= len(args) {
				return fmt.Errorf("--port requires a value")
			}
			i++
			port = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire serve [--port 8080]", args[i])
		}
	}

	// Resolve webhook secret from keychain or env
	secret := webhookSecret()
	if secret == "" {
		log.Printf("[serve] warning: no webhook secret configured — signature verification disabled")
		log.Printf("[serve] run 'spire connect linear' or set LINEAR_WEBHOOK_SECRET env var")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		handleWebhook(w, r, secret)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"ok":true}`)
	})

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("[serve] shutting down")
		server.Close()
	}()

	log.Printf("[serve] listening on :%s", port)
	err := server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func handleWebhook(w http.ResponseWriter, r *http.Request, secret string) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}

	// Verify signature if secret is configured
	if secret != "" {
		signature := r.Header.Get("Linear-Signature")
		if signature == "" {
			log.Printf("[serve] missing Linear-Signature header")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if !verifyHMAC(body, signature, secret) {
			log.Printf("[serve] invalid signature")
			http.Error(w, `{"error":"invalid signature"}`, http.StatusUnauthorized)
			return
		}
	}

	// Parse payload
	var payload struct {
		Action string `json:"action"`
		Type   string `json:"type"`
		Data   struct {
			Identifier string `json:"identifier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("[serve] invalid JSON: %s", err)
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	if payload.Data.Identifier == "" {
		log.Printf("[serve] missing data.identifier")
		http.Error(w, `{"error":"missing data.identifier"}`, http.StatusBadRequest)
		return
	}

	eventType := payload.Type + "." + payload.Action
	id := randomID()

	// Write directly to local Dolt webhook_queue
	_, err = doltSQL(
		fmt.Sprintf(
			"INSERT INTO webhook_queue (id, event_type, linear_id, payload, processed, created_at) VALUES ('%s', '%s', '%s', '%s', 0, NOW())",
			escSQL(id), escSQL(eventType), escSQL(payload.Data.Identifier), escSQL(string(body)),
		),
		false,
	)
	if err != nil {
		log.Printf("[serve] queue write failed: %s", err)
		// Still return 200 to Linear to prevent retries
	} else {
		log.Printf("[serve] queued %s for %s (id=%s)", eventType, payload.Data.Identifier, id)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"id":"%s"}`, id)
}

func verifyHMAC(body []byte, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}

func escSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// webhookSecret resolves the webhook signing secret.
// Priority: LINEAR_WEBHOOK_SECRET env > system keychain.
func webhookSecret() string {
	if s := os.Getenv("LINEAR_WEBHOOK_SECRET"); s != "" {
		return s
	}
	s, _ := keychainGet("linear.webhook-secret")
	return s
}
