package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"

	"golang.org/x/net/html"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Engines in the order they are asked: Wikipedia and Grokipedia, then the others shuffled
func order() []engine {
	others := []engine{
		{"brave", brave},
		{"bing", duckduckgo},
		{"google", google},
		{"mojeek", mojeek},
		{"google", startpage},
		{"bing", yahoo},
	}
	rand.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })
	return append([]engine{{"wikipedia", wikipedia}, {"grokipedia", grokipedia}}, others...)
}

// Results of a page of hits: every element of the page that is an item, read into a result
func scrape(text string, items test, fields func(item *html.Node) result) ([]result, error) {
	doc, err := parse(text)
	if err != nil {
		return nil, err
	}
	var results []result
	for _, item := range elements(doc, items) {
		results = append(results, fields(item))
	}
	return results, nil
}

// Brave's own index
func brave(ctx context.Context, b *browser, q query) ([]result, error) {
	country, _ := split(strings.ToLower(q.region))
	cookies := [][2]string{{country, country}, {"useLocation", "0"}}
	switch q.safesearch {
	case "on":
		cookies = append(cookies, [2]string{"safesearch", "strict"})
	case "off":
		cookies = append(cookies, [2]string{"safesearch", "off"})
	}
	b.cookies("https://search.brave.com", cookies)
	params := [][2]string{{"q", q.text}, {"source", "web"}}
	if q.timelimit != "" {
		params = append(params, [2]string{"tf", "p" + q.timelimit})
	}
	text, err := b.page(ctx, http.MethodGet, "https://search.brave.com/search", params, nil)
	if err != nil || text == "" {
		return nil, err
	}
	return scrape(text, and(el("div"), valued("data-type", "web")), func(item *html.Node) result {
		return hit(
			texts(item, within(item, and(el("div"), classed("title", "sitename-container"), last))),
			values(item, "href", and(el("a"), holding(and(el("div"), classed("title"))))),
			texts(item, within(item, and(el("div"), classed("content"), within(item, and(el("div"), classed("snippet")))))),
		)
	})
}

// DuckDuckGo's HTML page, served from Bing's index; ads left out
func duckduckgo(ctx context.Context, b *browser, q query) ([]result, error) {
	form := [][2]string{{"q", q.text}, {"b", ""}, {"l", q.region}}
	if q.timelimit != "" {
		form = append(form, [2]string{"df", q.timelimit})
	}
	text, err := b.page(ctx, http.MethodPost, "https://html.duckduckgo.com/html/", nil, form)
	if err != nil || text == "" {
		return nil, err
	}
	results, err := scrape(text, and(el("div"), classed("body")), func(item *html.Node) result {
		return hit(
			texts(item, within(item, el("h2"))),
			values(item, "href", and(el("a"), childOf(item))),
			texts(item, within(item, and(el("a"), childOf(item)))),
		)
	})
	return slices.DeleteFunc(results, func(r result) bool { return strings.HasPrefix(r.href, "https://duckduckgo.com/y.js?") }), err
}

// Google's WML pages, their redirect links unwrapped
func google(ctx context.Context, b *browser, q query) ([]result, error) {
	b.header.Set("User-Agent", nokia())
	b.cookies("https://google.com", [][2]string{{"CONSENT", "YES+"}})
	country, language := split(q.region)
	filter := map[string]string{"on": "2", "moderate": "1", "off": "0"}[q.safesearch]
	params := [][2]string{
		{"q", q.text}, {"sca_esv", "1"}, {"filter", filter}, {"start", "0"},
		{"hl", language + "-" + strings.ToUpper(country)}, {"lr", "lang_" + language}, {"cr", "country" + strings.ToUpper(country)},
	}
	if q.timelimit != "" {
		params = append(params, [2]string{"tbs", "qdr:" + q.timelimit})
	}
	text, err := b.page(ctx, http.MethodGet, "https://www.google.com/wml/search", params, nil)
	// The page is read from the end of its XML declaration, and has nothing without one
	declared := strings.Index(text, "?>")
	if err != nil || declared < 0 {
		return nil, err
	}
	items := and(el("div"), child("div", 1, holding(el("a"))), child("div", 2, holding(el("table"))))
	results, err := scrape(text[declared:], items, func(item *html.Node) result {
		link := and(el("a"), parent(and(el("div"), childOf(item))))
		return hit(
			texts(item, parent(and(el("span"), at(1), parent(link)))),
			values(item, "href", link),
			texts(item, within(item, and(el("div"), childOf(item), at(2), holding(el("table"))))),
		)
	})
	if err != nil {
		return nil, err
	}
	var kept []result
	for _, r := range results {
		if target, ok := strings.CutPrefix(r.href, "/url?q="); ok {
			if end := strings.IndexAny(target, "&?"); end >= 0 {
				target = target[:end]
			}
			r.href = normalURL(target)
		}
		if r.title != "" && strings.HasPrefix(r.href, "http") {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// User agent of the old Nokia phone Google's WML pages are asked for by
func nokia() string {
	firmware := []string{"2.0617.1.0.3", "2.0625.2.0.2", "2.0635.2.0.2", "5.0706.4.0.1", "5.0819.4.0.1"}
	agent := "NokiaN72/" + firmware[rand.IntN(len(firmware))] + " Series60/2.8 Profile/MIDP-2.0 Configuration/CLDC-1.1"
	if rand.IntN(2) == 0 {
		uc := []string{"7.9.1.120", "7.9.1.121", "7.9.1.122"}
		agent += "/UC Browser" + uc[rand.IntN(len(uc))] + "/27/351/UCWEB"
	}
	return agent
}

// Grokipedia's closest article, its snippet past the title paragraph
func grokipedia(ctx context.Context, b *browser, q query) ([]result, error) {
	text, err := b.page(ctx, http.MethodGet, "https://grokipedia.com/api/typeahead", [][2]string{{"query", q.text}, {"limit", "1"}}, nil)
	if err != nil || text == "" {
		return nil, err
	}
	var answer struct {
		Results []struct {
			Title   string  `json:"title"`
			Snippet string  `json:"snippet"`
			Slug    *string `json:"slug"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &answer); err != nil {
		return nil, err
	}
	if len(answer.Results) == 0 {
		return nil, nil
	}
	article := answer.Results[0]
	if article.Slug == nil {
		return nil, errors.New("the Grokipedia answer names no page")
	}
	body := article.Snippet
	if _, rest, ok := strings.Cut(body, "\n\n"); ok {
		body = rest
	}
	return []result{hit(strings.Trim(article.Title, "_"), "https://grokipedia.com/page/"+*article.Slug, body)}, nil
}

// Mojeek's own index
func mojeek(ctx context.Context, b *browser, q query) ([]result, error) {
	country, language := split(strings.ToLower(q.region))
	b.cookies("https://www.mojeek.com", [][2]string{{"arc", country}, {"lb", language}})
	params := [][2]string{{"q", q.text}}
	if q.safesearch == "on" {
		params = append(params, [2]string{"safe", "1"})
	}
	text, err := b.page(ctx, http.MethodGet, "https://www.mojeek.com/search", params, nil)
	if err != nil || text == "" {
		return nil, err
	}
	return scrape(text, and(el("li"), parent(and(el("ul"), classed("results")))), func(item *html.Node) result {
		return hit(
			texts(item, within(item, el("h2"))),
			values(item, "href", and(el("a"), parent(el("h2")))),
			texts(item, within(item, and(el("p"), valued("class", "s")))),
		)
	})
}

// Startpage, served from Google's index
func startpage(ctx context.Context, b *browser, q query) ([]result, error) {
	b.header.Set("Referer", "https://www.startpage.com/")
	country, language := split(strings.ToLower(q.region))
	filter := map[string]string{"on": "heavy", "moderate": "moderate", "off": "none"}[q.safesearch]
	sc, err := startpageToken(ctx, b)
	if err != nil {
		return nil, err
	}
	form := [][2]string{
		{"query", q.text}, {"cat", "web"}, {"t", "device"}, {"sc", sc}, {"lui", "english"}, {"language", "english"},
		{"abp", "1"}, {"abd", "0"}, {"abe", "0"}, {"qsr", language + "_" + strings.ToUpper(country)}, {"qadf", filter}, {"segment", "organic"},
	}
	if q.timelimit != "" {
		form = append(form, [2]string{"with_date", q.timelimit})
	}
	text, err := b.page(ctx, http.MethodPost, "https://www.startpage.com/sp/search", nil, form)
	if err != nil || text == "" {
		return nil, err
	}
	return scrape(text, and(el("div"), classed("result"), holding(el("a"))), func(item *html.Node) result {
		return hit(
			texts(item, within(item, el("h2"))),
			values(item, "href", and(el("a"), childOf(item))),
			texts(item, within(item, el("p"))),
		)
	})
}

// Token the search form of Startpage's home page carries, empty when it has none
func startpageToken(ctx context.Context, b *browser) (string, error) {
	_, text, err := b.send(ctx, http.MethodGet, "https://www.startpage.com/", nil, nil)
	if err != nil {
		return "", err
	}
	doc, err := parse(text)
	if err != nil {
		return "", err
	}
	for _, input := range elements(doc, and(el("input"), valued("name", "sc"), within(doc, and(el("form"), valued("id", "search"))))) {
		if value, ok := attr(input, "value"); ok {
			return value, nil
		}
	}
	return "", nil
}

// Wikipedia's closest article with its introduction, none for a disambiguation page
func wikipedia(ctx context.Context, b *browser, q query) ([]result, error) {
	_, language := split(strings.ToLower(q.region))
	api := "https://" + language + ".wikipedia.org/w/api.php"
	text, err := b.page(ctx, http.MethodGet, api+"?action=opensearch&profile=fuzzy&limit=1&search="+quote(q.text), nil, nil)
	if err != nil || text == "" {
		return nil, err
	}
	var answer []json.RawMessage
	if err := json.Unmarshal([]byte(text), &answer); err != nil {
		return nil, err
	}
	var titles, links []string
	if err := entry(answer, 1, &titles); err != nil || len(titles) == 0 {
		return nil, err
	}
	if err := entry(answer, 3, &links); err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, errors.New("the Wikipedia search names no page")
	}
	article := hit(titles[0], links[0], "")
	text, err = b.page(ctx, http.MethodGet, api+"?action=query&format=json&prop=extracts&titles="+quote(article.title)+"&explaintext=0&exintro=0&redirects=1", nil, nil)
	if err != nil {
		return nil, err
	}
	if text != "" {
		extract, err := introduction(text)
		if err != nil {
			return nil, err
		}
		article.body = normalText(extract)
	}
	if strings.Contains(article.body, "may refer to:") {
		return nil, nil
	}
	return []result{article}, nil
}

// Entry of an opensearch answer read into v
func entry(answer []json.RawMessage, i int, v any) error {
	if i >= len(answer) {
		return errors.New("the Wikipedia search answer is incomplete")
	}
	return json.Unmarshal(answer[i], v)
}

// Extract of the first page a Wikipedia query answer lists
func introduction(text string) (string, error) {
	var answer struct {
		Query struct {
			Pages json.RawMessage `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal([]byte(text), &answer); err != nil {
		return "", err
	}
	pages := json.NewDecoder(bytes.NewReader(answer.Query.Pages))
	if open, err := pages.Token(); err != nil || open != json.Delim('{') || !pages.More() {
		return "", errors.New("the Wikipedia answer lists no page")
	}
	if _, err := pages.Token(); err != nil {
		return "", err
	}
	var page struct {
		Extract string `json:"extract"`
	}
	if err := pages.Decode(&page); err != nil {
		return "", err
	}
	return page.Extract, nil
}

// Yahoo, served from Bing's index; ads left out, its redirect links unwrapped
func yahoo(ctx context.Context, b *browser, q query) ([]result, error) {
	params := [][2]string{{"p", q.text}}
	if q.timelimit != "" {
		params = append(params, [2]string{"btf", q.timelimit})
	}
	text, err := b.page(ctx, http.MethodGet, "https://search.yahoo.com/search;_ylt="+strs.Token(18)+";_ylu="+strs.Token(35), params, nil)
	if err != nil || text == "" {
		return nil, err
	}
	heading := and(el("div"), classed("Title"))
	results, err := scrape(text, and(el("div"), classed("relsrch")), func(item *html.Node) result {
		return hit(
			texts(item, within(item, and(el("h3"), within(item, heading)))),
			values(item, "href", and(el("a"), within(item, heading))),
			texts(item, within(item, and(el("div"), classed("Text")))),
		)
	})
	if err != nil {
		return nil, err
	}
	var kept []result
	for _, r := range results {
		if strings.HasPrefix(r.href, "https://www.bing.com/aclick?") {
			continue
		}
		if _, target, ok := strings.Cut(r.href, "/RU="); ok {
			target, _, _ = strings.Cut(target, "/RK=")
			target, _, _ = strings.Cut(target, "/RS=")
			r.href = normalURL(unquote(strings.ReplaceAll(target, "+", " ")))
		}
		kept = append(kept, r)
	}
	return kept, nil
}
