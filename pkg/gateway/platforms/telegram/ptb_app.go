package telegram

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// SDK bot with its request pair and long poll updater, the PTB Application built per connect
type ptbApp struct {
	sdk     *bot.Bot
	general *ptbRequest
	polling *ptbRequest
	updater *sdkUpdater

	mu          sync.Mutex
	initialized bool
	botUser     *models.User
	client      *botHTTPClient
	fileBase    string
	localMode   bool
	serverURL   string
}

// Application over the server URL whose updates, polled or pushed, reach deliver in order
func newPTBApp(token, serverURL string, general, polling *ptbRequest, deliver func(ctx context.Context, update *models.Update)) (*ptbApp, error) {
	client := &botHTTPClient{general: general, polling: polling}
	sdk, err := bot.New(token, bot.WithServerURL(serverURL), bot.WithHTTPClient(0, client),
		bot.WithSkipGetMe(), bot.WithNotAsyncHandlers(),
		bot.WithDefaultHandler(func(ctx context.Context, _ *bot.Bot, update *models.Update) { deliver(ctx, update) }),
		bot.WithErrorsHandler(func(err error) { slog.Debug("Telegram SDK error", "error", ptbError(err)) }))
	if err != nil {
		return nil, err
	}
	app := &ptbApp{sdk: sdk, general: general, polling: polling, client: client, serverURL: strings.TrimRight(serverURL, "/"),
		fileBase: strings.TrimRight(serverURL, "/") + "/file/bot" + token}
	app.updater = &sdkUpdater{bot: sdkPollingBot{b: sdk}, request: polling, pollTimeout: 10,
		updatesURL: strings.TrimRight(serverURL, "/") + "/bot" + token + "/getUpdates",
		deliver:    func(ctx context.Context, update *models.Update) { sdk.ProcessUpdate(ctx, update) }}
	return app, nil
}

// Initializes both request pools and caches the bot user from getMe
func (a *ptbApp) Initialize(ctx context.Context) error {
	a.mu.Lock()
	if a.initialized {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	for _, request := range []*ptbRequest{a.polling, a.general} {
		if err := request.Initialize(ctx); err != nil {
			return err
		}
	}
	user, err := a.sdk.GetMe(ctx)
	if err != nil {
		return ptbError(err)
	}
	a.mu.Lock()
	a.initialized, a.botUser = true, user
	a.mu.Unlock()
	return nil
}

// Shuts both request pools, a no op for an application that never initialized
func (a *ptbApp) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	if !a.initialized {
		a.mu.Unlock()
		slog.Debug("This Application is already shut down. Returning.")
		return nil
	}
	a.initialized = false
	a.mu.Unlock()
	for _, request := range []*ptbRequest{a.polling, a.general} {
		if err := request.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *ptbApp) PollingBot() pollingBot { return sdkPollingBot{b: a.sdk} }

func (a *ptbApp) Updater() pollingUpdater { return a.updater }

func (a *ptbApp) Requests() (requestPool, requestPool) { return a.polling, a.general }

// PTB base url of a token, the {token} marker filled or the token appended
func ptbParseBaseURL(value, token string) string {
	if strings.Contains(value, "{token}") {
		return strings.ReplaceAll(value, "{token}", token)
	}
	return value + token
}

// Points every Bot API call and file download at the custom server the way the PTB builder base_url does
func (a *ptbApp) useBaseURLs(token, baseURL, baseFileURL string, localMode bool) {
	apiBase := ptbParseBaseURL(baseURL, token)
	a.client.apiBase = apiBase
	a.updater.updatesURL = apiBase + "/getUpdates"
	a.fileBase = ptbParseBaseURL(baseFileURL, token)
	a.localMode = localMode
}

// Reads local mode files from disk and fetches the rest from the file endpoint, the PTB File download
func (a *ptbApp) downloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	file, err := a.sdk.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, "", ptbError(err)
	}
	path := file.FilePath
	if info, statErr := os.Stat(path); path != "" && statErr == nil && info.Mode().IsRegular() {
		data, readErr := os.ReadFile(path)
		return data, path, readErr
	}
	if a.localMode {
		data, readErr := os.ReadFile(path)
		return data, path, readErr
	}
	target := a.fileBase + "/" + path
	req, err := http.NewRequestWithContext(withRawRetrieve(ctx), http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := a.general.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, target, err
}
