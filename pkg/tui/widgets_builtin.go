package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tickerPoints = 26
	tickerPip    = 0.0001
)

var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// Series as one row of block characters scaled between its lowest and highest value
func sparkline(series []float64) string {
	if len(series) == 0 {
		return ""
	}
	lo, hi := series[0], series[0]
	for _, v := range series {
		lo, hi = min(lo, v), max(hi, v)
	}
	out := make([]rune, len(series))
	for i, v := range series {
		level := 0
		if hi > lo {
			level = int((v - lo) / (hi - lo) * float64(len(sparkBlocks)-1))
		}
		out[i] = sparkBlocks[level]
	}
	return string(out)
}

// One span line of a widget view
func widgetText(role, text string) WidgetLine {
	return WidgetLine{Spans: []WidgetSpan{{Text: text, Role: role}}}
}

// Whether an input closes a widget, escape, q or the interrupt key
func closesWidget(in WidgetInput) bool {
	return in.Key == "esc" || in.Text == "q" || in.Key == "ctrl+c"
}

// Random walk price of a symbol
type tickerState struct {
	symbol string
	series []float64
}

// Weather report of a place, filled by a fetch that runs beside the UI
type weatherState struct {
	mu        sync.Mutex
	location  string
	loading   bool
	failure   string
	area      string
	condition string
	art       string
	temp      float64
	feels     float64
	humidity  float64
	wind      float64
}

var weatherArt = map[string][]string{
	"sun":     {`    \   /    `, `     .-.     `, `  ― (   ) ―  `, "     `-'     ", `    /   \    `},
	"cloud":   {`             `, `     .--.    `, `  .-(    ).  `, ` (___.__)__) `, `             `},
	"fog":     {`             `, ` _ - _ - _ - `, `  _ - _ - _  `, ` _ - _ - _ - `, `             `},
	"rain":    {`     .-.     `, `    (   ).   `, `   (___(__)  `, `  ‚ʻ‚ʻ‚ʻ‚ʻ   `, `  ‚ʻ‚ʻ‚ʻ‚ʻ   `},
	"snow":    {`     .-.     `, `    (   ).   `, `   (___(__)  `, `   * * * *   `, `  * * * *    `},
	"thunder": {`     .-.     `, `    (   ).   `, `   (___(__)  `, `  ⚡‚ʻ⚡‚ʻ   `, `  ‚ʻ⚡‚ʻ⚡   `},
}

var weatherArtRole = map[string]string{"cloud": "muted", "fog": "muted", "rain": "shellDollar", "snow": "text", "sun": "primary", "thunder": "warn"}

var weatherConditions = map[int]string{0: "Clear sky", 1: "Mainly clear", 2: "Partly cloudy", 3: "Overcast", 45: "Fog", 48: "Rime fog",
	51: "Light drizzle", 53: "Drizzle", 55: "Heavy drizzle", 56: "Light freezing drizzle", 57: "Freezing drizzle", 61: "Light rain", 63: "Rain", 65: "Heavy rain",
	66: "Light freezing rain", 67: "Freezing rain", 71: "Light snow", 73: "Snow", 75: "Heavy snow", 77: "Snow grains", 80: "Light rain showers", 81: "Rain showers",
	82: "Heavy rain showers", 85: "Light snow showers", 86: "Heavy snow showers", 95: "Thunderstorm", 96: "Thunderstorm with hail", 99: "Severe thunderstorm with hail"}

// Picture of a weather code
func weatherArtFor(code int) string {
	switch {
	case code == 0:
		return "sun"
	case code == 45 || code == 48:
		return "fog"
	case (code >= 51 && code <= 67) || (code >= 80 && code <= 82):
		return "rain"
	case (code >= 71 && code <= 77) || code == 85 || code == 86:
		return "snow"
	case code >= 95:
		return "thunder"
	}
	return "cloud"
}

// JSON of a weather service answer
func weatherGet(ctx context.Context, target string, into any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "icebath-tui-weather")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", req.URL.Host, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(into)
}

// Fetches the place and its current conditions from Open-Meteo, the place from the address of this machine when none is named
func (w *weatherState) fetch(ctx context.Context) {
	var lat, lon float64
	area, zone := "", "auto"
	if w.location != "" {
		var found struct {
			Results []struct {
				Name      string  `json:"name"`
				Country   string  `json:"country"`
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
				Timezone  string  `json:"timezone"`
			} `json:"results"`
		}
		query := url.Values{"count": {"1"}, "format": {"json"}, "language": {"en"}, "name": {w.location}}
		if err := weatherGet(ctx, "https://geocoding-api.open-meteo.com/v1/search?"+query.Encode(), &found); err != nil {
			w.fail(err.Error())
			return
		}
		if len(found.Results) == 0 {
			w.fail("location not found: " + w.location)
			return
		}
		place := found.Results[0]
		lat, lon, area = place.Latitude, place.Longitude, strings.Trim(place.Name+", "+place.Country, ", ")
		if place.Timezone != "" {
			zone = place.Timezone
		}
	} else {
		var here struct {
			Success   bool    `json:"success"`
			City      string  `json:"city"`
			Country   string  `json:"country"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Timezone  struct {
				ID string `json:"id"`
			} `json:"timezone"`
		}
		if err := weatherGet(ctx, "https://ipwho.is/?fields=success,city,country,latitude,longitude,timezone", &here); err != nil {
			w.fail(err.Error())
			return
		}
		if !here.Success {
			w.fail("this machine's location is unknown, name a place: /weather <location>")
			return
		}
		lat, lon, area = here.Latitude, here.Longitude, strings.Trim(here.City+", "+here.Country, ", ")
		if here.Timezone.ID != "" {
			zone = here.Timezone.ID
		}
	}
	var report struct {
		Current struct {
			Temperature float64 `json:"temperature_2m"`
			Feels       float64 `json:"apparent_temperature"`
			Humidity    float64 `json:"relative_humidity_2m"`
			Code        int     `json:"weather_code"`
			Wind        float64 `json:"wind_speed_10m"`
		} `json:"current"`
	}
	query := url.Values{"latitude": {strconv.FormatFloat(lat, 'f', 4, 64)}, "longitude": {strconv.FormatFloat(lon, 'f', 4, 64)}, "timezone": {zone},
		"current": {"temperature_2m,apparent_temperature,relative_humidity_2m,weather_code,wind_speed_10m"}}
	if err := weatherGet(ctx, "https://api.open-meteo.com/v1/forecast?"+query.Encode(), &report); err != nil {
		w.fail(err.Error())
		return
	}
	condition := weatherConditions[report.Current.Code]
	if condition == "" {
		condition = "Weather code " + strconv.Itoa(report.Current.Code)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loading, w.area, w.condition, w.art = false, area, condition, weatherArtFor(report.Current.Code)
	w.temp, w.feels, w.humidity, w.wind = report.Current.Temperature, report.Current.Feels, report.Current.Humidity, report.Current.Wind
}

func (w *weatherState) fail(reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loading, w.failure = false, reason
}

// Demo grid, its size, the cell under the cursor, the marked cells and the live series of the streams mode
type gridState struct {
	cols, rows int
	x, y       int
	marked     map[[2]int]bool
	streams    bool
	series     map[[2]int][]float64
}

// Size of a demo grid from 4x3, 4 3 or one number of columns
func parseGrid(arg string) (cols, rows int, ok bool) {
	cols, rows = 3, 2
	fields := strings.FieldsFunc(strings.ToLower(arg), func(r rune) bool { return r == 'x' || r == ' ' || r == '×' })
	if len(fields) > 2 {
		return 0, 0, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > 12 {
			return 0, 0, false
		}
		if i == 0 {
			cols = n
		} else {
			rows = n
		}
	}
	return cols, rows, true
}

var dialogZones = []string{"center", "top", "bottom", "left", "right", "top-left", "top-right", "bottom-left", "bottom-right"}

func init() {
	for _, w := range []Widget{
		{ID: "ticker", Help: "fake 1-pip chart with a live sparkline", Usage: "usage: /ticker [symbol]", Mode: "ambient", Width: tickerPoints + 6, Every: 250 * time.Millisecond,
			Open: func(_ context.Context, arg string) (any, bool, error) {
				symbol := "ICEB"
				if fields := strings.Fields(arg); len(fields) > 0 {
					symbol = strings.ToUpper(fields[0])
				}
				if len(symbol) > 8 {
					symbol = symbol[:8]
				}
				series := []float64{1.1 + rand.Float64()*0.4}
				for len(series) < tickerPoints {
					series = append(series, series[len(series)-1]+(rand.Float64()-0.5)*4*tickerPip)
				}
				return &tickerState{symbol: symbol, series: series}, true, nil
			},
			Tick: func(_ context.Context, state any) (any, error) {
				s := state.(*tickerState)
				s.series = append(s.series[1:], s.series[len(s.series)-1]+(rand.Float64()-0.5)*4*tickerPip)
				return s, nil
			},
			Render: func(_ context.Context, frame WidgetFrame) (WidgetView, error) {
				s := frame.State.(*tickerState)
				price := s.series[len(s.series)-1]
				delta := price - s.series[len(s.series)-2]
				role, arrow := "ok", "▲"
				if delta < 0 {
					role, arrow, delta = "error", "▼", -delta
				}
				head := WidgetLine{Spans: []WidgetSpan{{Text: s.symbol + " ", Role: "label", Bold: true}, {Text: strconv.FormatFloat(price, 'f', 4, 64) + " "},
					{Text: arrow + strconv.FormatFloat(delta/tickerPip, 'f', 1, 64) + "p", Role: role}}}
				return WidgetView{Lines: []WidgetLine{head, widgetText(role, sparkline(s.series))}}, nil
			}},
		{ID: "weather", Help: "current conditions with ASCII art (Open-Meteo)", Usage: "usage: /weather [location]   (blank = locate by address)", Mode: "ambient", Width: 44, Every: 500 * time.Millisecond,
			Open: func(ctx context.Context, arg string) (any, bool, error) {
				w := &weatherState{location: strings.TrimSpace(arg), loading: true}
				go w.fetch(ctx)
				return w, true, nil
			},
			Tick: func(_ context.Context, state any) (any, error) { return state, nil },
			Render: func(_ context.Context, frame WidgetFrame) (WidgetView, error) {
				w := frame.State.(*weatherState)
				w.mu.Lock()
				defer w.mu.Unlock()
				switch {
				case w.loading:
					place := w.location
					if place == "" {
						place = "your location"
					}
					return WidgetView{Lines: []WidgetLine{widgetText("muted", "fetching the weather for "+place+"…")}}, nil
				case w.failure != "":
					return WidgetView{Lines: []WidgetLine{widgetText("error", "⚠ "+w.failure)}}, nil
				}
				facts := []string{w.area, w.condition, fmt.Sprintf("%.0f°C, feels like %.0f°C", w.temp, w.feels), fmt.Sprintf("wind %.0f km/h", w.wind), fmt.Sprintf("humidity %.0f%%", w.humidity)}
				var lines []WidgetLine
				for i, row := range weatherArt[w.art] {
					role := "text"
					if i == 0 {
						role = "label"
					}
					lines = append(lines, WidgetLine{Spans: []WidgetSpan{{Text: row + " ", Role: weatherArtRole[w.art]}, {Text: facts[i], Role: role, Bold: i == 0}}})
				}
				return WidgetView{Lines: lines}, nil
			}},
		{ID: "grid-test", Help: "open an interactive widget grid demo", Usage: "usage: /grid-test [cols]x[rows]  ·  /grid-test [cols] [rows]  ·  /grid-test streams", Mode: "modal", Every: 300 * time.Millisecond,
			Open: func(_ context.Context, arg string) (any, bool, error) {
				g := &gridState{marked: map[[2]int]bool{}, series: map[[2]int][]float64{}}
				if strings.EqualFold(strings.TrimSpace(arg), "streams") {
					g.cols, g.rows, g.streams = 3, 2, true
					return g, true, nil
				}
				cols, rows, ok := parseGrid(arg)
				g.cols, g.rows = cols, rows
				return g, ok, nil
			},
			Tick: func(_ context.Context, state any) (any, error) {
				g := state.(*gridState)
				if !g.streams {
					return g, nil
				}
				for y := range g.rows {
					for x := range g.cols {
						cell := [2]int{x, y}
						series := g.series[cell]
						last := 0.5
						if len(series) > 0 {
							last = series[len(series)-1]
						}
						series = append(series, min(max(last+(rand.Float64()-0.5)*0.2, 0), 1))
						if len(series) > 40 {
							series = series[1:]
						}
						g.series[cell] = series
					}
				}
				return g, nil
			},
			Input: func(_ context.Context, state any, in WidgetInput) (any, bool, error) {
				g := state.(*gridState)
				switch {
				case closesWidget(in):
					return g, false, nil
				case in.Key == "left" || in.Text == "h":
					g.x = max(g.x-1, 0)
				case in.Key == "right" || in.Text == "l":
					g.x = min(g.x+1, g.cols-1)
				case in.Key == "up" || in.Text == "k":
					g.y = max(g.y-1, 0)
				case in.Key == "down" || in.Text == "j":
					g.y = min(g.y+1, g.rows-1)
				case in.Key == "enter" || in.Key == "space" || in.Text == " ":
					g.marked[[2]int{g.x, g.y}] = !g.marked[[2]int{g.x, g.y}]
				case in.Text == "+":
					g.cols = min(g.cols+1, 12)
				case in.Text == "-":
					g.cols = max(g.cols-1, 1)
				case in.Text == "]":
					g.rows = min(g.rows+1, 12)
				case in.Text == "[":
					g.rows = max(g.rows-1, 1)
				case in.Text == "s":
					g.streams = !g.streams
				}
				g.x, g.y = min(g.x, g.cols-1), min(g.y, g.rows-1)
				return g, true, nil
			},
			Render: func(_ context.Context, frame WidgetFrame) (WidgetView, error) {
				g := frame.State.(*gridState)
				cellW := max((frame.Cols-1)/g.cols-1, 4)
				cellH := max((frame.Rows-2)/g.rows-1, 1)
				var lines []WidgetLine
				rule := "+" + strings.Repeat(strings.Repeat("-", cellW)+"+", g.cols)
				for y := range g.rows {
					lines = append(lines, widgetText("border", rule))
					for row := range cellH {
						line := WidgetLine{Spans: []WidgetSpan{{Text: "|", Role: "border"}}}
						for x := range g.cols {
							cell := [2]int{x, y}
							text := ""
							if row == cellH/2 {
								text = fmt.Sprintf("%d,%d", x+1, y+1)
								if g.streams {
									series := g.series[cell]
									text = sparkline(series[max(len(series)-cellW+2, 0):])
								}
								if g.marked[cell] {
									text = "● " + text
								}
							}
							pad := max(cellW-len([]rune(text)), 0)
							span := WidgetSpan{Text: strings.Repeat(" ", pad/2) + text + strings.Repeat(" ", pad-pad/2), Role: "text"}
							if x == g.x && y == g.y {
								span.Inverse, span.Role = true, "accent"
							}
							line.Spans = append(line.Spans, span, WidgetSpan{Text: "|", Role: "border"})
						}
						lines = append(lines, line)
					}
				}
				lines = append(lines, widgetText("border", rule))
				return WidgetView{Lines: append(lines, widgetText("muted", "arrows move · space marks · + - columns · [ ] rows · s streams · q closes"))}, nil
			}},
		{ID: "dialog-test", Help: "open a sample dialog over a faked backdrop", Usage: "usage: /dialog-test [zone]   zones: " + strings.Join(dialogZones, ", "), Mode: "modal",
			Open: func(_ context.Context, arg string) (any, bool, error) {
				zone := strings.ToLower(strings.TrimSpace(arg))
				if zone == "" {
					zone = "center"
				}
				for _, known := range dialogZones {
					if known == zone {
						return zone, true, nil
					}
				}
				return nil, false, nil
			},
			Input: func(_ context.Context, state any, in WidgetInput) (any, bool, error) {
				return state, !closesWidget(in) && in.Key != "enter", nil
			},
			Render: func(_ context.Context, frame WidgetFrame) (WidgetView, error) {
				zone := frame.State.(string)
				body := []string{"Sample dialog · " + zone, "", "Windows of the terminal UI open in this zone.", "", "enter or esc closes"}
				width := 0
				for _, row := range body {
					width = max(width, len([]rune(row)))
				}
				width = min(width+4, frame.Cols)
				height := len(body) + 2
				left, top := (frame.Cols-width)/2, (frame.Rows-height)/2
				if strings.Contains(zone, "left") {
					left = 1
				}
				if strings.Contains(zone, "right") {
					left = frame.Cols - width - 1
				}
				if strings.HasPrefix(zone, "top") {
					top = 0
				}
				if strings.HasPrefix(zone, "bottom") {
					top = frame.Rows - height
				}
				left, top = max(left, 0), max(top, 0)
				lines := make([]WidgetLine, 0, frame.Rows)
				for y := range frame.Rows {
					if y < top || y >= top+height {
						lines = append(lines, WidgetLine{Spans: []WidgetSpan{{Text: strings.Repeat("░", frame.Cols), Role: "border", Dim: true}}})
						continue
					}
					inner := strings.Repeat("─", width-2)
					switch i := y - top; {
					case i == 0:
						inner = "╭" + inner + "╮"
					case i == height-1:
						inner = "╰" + inner + "╯"
					default:
						text := body[i-1]
						inner = "│ " + text + strings.Repeat(" ", max(width-4-len([]rune(text)), 0)) + " │"
					}
					lines = append(lines, WidgetLine{Spans: []WidgetSpan{{Text: strings.Repeat("░", left), Role: "border", Dim: true}, {Text: inner, Role: "text"},
						{Text: strings.Repeat("░", max(frame.Cols-left-width, 0)), Role: "border", Dim: true}}})
				}
				return WidgetView{Lines: lines}, nil
			}},
	} {
		if err := RegisterWidget(w); err != nil {
			panic(err)
		}
	}
}
