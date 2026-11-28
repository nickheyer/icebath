package edgetts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// List of the voices of the Edge browser's read aloud
const voicesEndpoint = "https://speech.platform.bing.com/consumer/speech/synthesize/readaloud/voices/list"

// One voice Edge reads aloud with
type Voice struct {
	ShortName    string
	FriendlyName string
	Locale       string
	Gender       string
}

// Voices Edge offers now, as its read aloud lists them; a refusal for a clock that drifted is asked again once with the clock of the service
func (c *Client) Voices(ctx context.Context) ([]Voice, error) {
	for attempt := 0; ; attempt++ {
		voices, response, err := c.voices(ctx)
		if response == nil || response.StatusCode != http.StatusForbidden || attempt > 0 || c.correctClock(response) != nil {
			return voices, err
		}
	}
}

// One read of the list of voices, with the reply when the service answered
func (c *Client) voices(ctx context.Context) ([]Voice, *http.Response, error) {
	query := url.Values{}
	query.Set("trustedclienttoken", trustedClientToken)
	query.Set("Sec-MS-GEC", secMSGEC(c.now()))
	query.Set("Sec-MS-GEC-Version", tokenVersion)
	bounded, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, voicesEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Cookie", "muid="+strings.ToUpper(randomHex(16))+";")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("the voices of Edge could not be read: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, response, fmt.Errorf("Edge answered the list of its voices with HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, response, err
	}
	var voices []Voice
	if err := json.Unmarshal(body, &voices); err != nil {
		return nil, response, fmt.Errorf("Edge sent a list of voices that can't be read: %w", err)
	}
	return voices, response, nil
}
