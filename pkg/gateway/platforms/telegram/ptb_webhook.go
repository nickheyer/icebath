package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Webhook listener of the updater, the PTB tornado WebhookServer
type webhookServer struct {
	srv  *http.Server
	done chan struct{}
}

// Webhook request handler validating the JSON content type and the secret token before queueing the update
func (u *sdkUpdater) webhookHandler(ctx context.Context, path, secretToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		slog.Debug("Webhook triggered")
		if r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if secretToken != "" {
			token := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
			if token == "" {
				slog.Debug("Request did not include the secret token")
				http.Error(w, "Request did not include the secret token", http.StatusForbidden)
				return
			}
			if token != secretToken {
				slog.Debug("Request had the wrong secret token")
				http.Error(w, "Request had the wrong secret token", http.StatusForbidden)
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Update could not be processed", http.StatusBadRequest)
			return
		}
		var update models.Update
		if err := json.Unmarshal(body, &update); err != nil {
			slog.Error("Exception in TelegramHandler", "error", err)
			http.Error(w, "Update could not be processed", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		u.deliver(context.WithoutCancel(ctx), &update)
	})
	return mux
}

// Serves the webhook endpoint then registers it with setWebhook, the PTB start_webhook bootstrap order
func (u *sdkUpdater) StartWebhook(ctx context.Context, listen string, port int, urlPath, webhookURL, secretToken string,
	dropPendingUpdates bool) error {
	u.lifecycle.Lock()
	defer u.lifecycle.Unlock()
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return &runtimeError{msg: "This Updater is already running!"}
	}
	u.running = true
	u.mu.Unlock()
	fail := func(err error) error {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
		return err
	}
	if urlPath == "" || urlPath[0] != '/' {
		urlPath = "/" + urlPath
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(listen, strconv.Itoa(port)))
	if err != nil {
		return fail(err)
	}
	server := &webhookServer{srv: &http.Server{Handler: u.webhookHandler(ctx, urlPath, secretToken)}, done: make(chan struct{})}
	go func() {
		defer close(server.done)
		if serveErr := server.srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			slog.Error("Error while serving the webhook", "error", serveErr)
		}
	}()
	params := &bot.SetWebhookParams{URL: webhookURL, AllowedUpdates: allUpdateTypes, DropPendingUpdates: dropPendingUpdates,
		SecretToken: secretToken, MaxConnections: 40}
	if _, err := u.bot.b.SetWebhook(ctx, params); err != nil {
		_ = server.srv.Close()
		<-server.done
		return fail(ptbError(err))
	}
	u.mu.Lock()
	u.webhook = server
	u.mu.Unlock()
	return nil
}

// Shuts the webhook listener down and waits for it to exit
func (u *sdkUpdater) stopWebhook(ctx context.Context, server *webhookServer) error {
	if err := server.srv.Shutdown(ctx); err != nil {
		_ = server.srv.Close()
	}
	select {
	case <-server.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
