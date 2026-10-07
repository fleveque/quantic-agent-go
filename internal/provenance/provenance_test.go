package provenance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fleveque/quantic-agent/internal/provenance"
)

// calendar120 is the real dividend_calendar result for 120 days from
// 2026-10-06, captured from quantic.finance: the data behind lesson 05's
// answer that claimed "180 days".
func calendar120(t *testing.T) *provenance.Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "calendar-120.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := provenance.NewManifest(provenance.Record{Tool: "dividend_calendar", Result: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// manifest builds one from literal JSON results, for cases that need a value
// the real calendar doesn't have (amounts, yields).
func manifest(t *testing.T, results ...string) *provenance.Manifest {
	t.Helper()
	var records []provenance.Record
	for _, r := range results {
		records = append(records, provenance.Record{Tool: "test", Result: r})
	}
	m, err := provenance.NewManifest(records...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func texts(fs []provenance.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Text)
	}
	return out
}

func TestCheckProse(t *testing.T) {
	cal := calendar120(t)
	quote := manifest(t, `{"symbol":"O","amount":0.2695,"yield_pct":5.42,"market_cap":1234.5,"change_pct":-3.5,"rank":1}`)

	tests := []struct {
		name string
		m    *provenance.Manifest
		text string
		want []string // the figures reported, in order; nil means all accounted for
	}{
		// The case that motivated this milestone: 120 days of data, 180 claimed.
		{"the claim from lesson 05", cal, "here are the companies that have ex-dividend dates within the next 180 days", []string{"180"}},
		{"the window the data covers", cal, "Here are the stocks going ex-dividend in the next 120 days", nil},
		{"a derived figure", cal, "in the next 4 months", []string{"4"}},

		{"ISO date", cal, "MSFT goes ex-dividend on 2026-10-08.", nil},
		{"ISO date not returned", cal, "MSFT goes ex-dividend on 2026-10-07.", []string{"2026-10-07"}},
		{"month and day", cal, "Microsoft (Oct 8) and Apple (Oct 9)", nil},
		{"month and day not returned", cal, "Apple on Oct 11", []string{"Oct 11"}},
		{"full date", cal, "on October 8, 2026", nil},
		{"day before month", cal, "on 8 October 2026", nil},
		{"ordinal day", cal, "on October 16th", nil},
		{"the day's number isn't checked twice", cal, "Oct 21 is busy", nil},
		{"a second day sharing the month", cal, "on October 16 and 17", nil},
		{"a second day that wasn't returned", cal, "on October 17 and 18", []string{"October 17 and 18"}},
		{"a day pair before the month", cal, "on 16 & 17 October", nil},

		// Numbers in words are figures too. A list's length counts as
		// returned: the calendar has ten stocks.
		{"a count that matches the list", cal, "ten stocks go ex-dividend", nil},
		{"a count that doesn't", cal, "eleven stocks go ex-dividend", []string{"eleven"}},
		{"a derived duration in words", cal, "about four months", []string{"four"}},
		{"a compound number", quote, "twenty-one days", []string{"twenty-one"}},
		{"a word number as an adjective", cal, "a six-month window", []string{"six"}},
		{"one is a pronoun, not a figure", cal, "one of them pays monthly", nil},

		{"year of a returned date", cal, "the 2026 calendar", nil},
		{"year with no returned date", cal, "by 2027", []string{"2027"}},

		{"exact amount", quote, "pays 0.2695 per share", nil},
		{"rounded amount", quote, "pays about 0.27 per share", []string{"0.27"}},
		{"currency symbol", quote, "pays $0.2695", nil},
		{"percent", quote, "yields 5.42%", nil},
		{"thousands separator", quote, "a market cap of 1,234.5", nil},
		{"negative percent", quote, "fell -3.5%", nil},

		{"digits inside words", cal, "Q3 results, week W38, a 3M position, qwen3.5 wrote it", nil},
		{"a hyphenated code", cal, "unlike COVID-19", nil},
		{"list positions", cal, "1. Realty Income\n2. Microsoft\n10. Diageo", nil},
		{"a number at a line start that isn't a list", cal, "180 days ahead", []string{"180"}},
		{"several, in order", cal, "In 3 weeks, on Oct 30, 12 companies", []string{"3", "Oct 30", "12"}},
		{"no figures at all", cal, "Utilities and healthcare names lead the week.", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := texts(provenance.CheckProse(tt.text, tt.m))
			if !slices.Equal(got, tt.want) {
				t.Errorf("CheckProse(%q) reported %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// A real answer from lesson 05, checked against the real data it was given:
// every date and the window all trace to the tool result.
func TestARealAnswerPasses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "llm", "testdata", "chat-tool-answer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Message struct{ Content string } `json:"message"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	sse, err := os.ReadFile(filepath.Join("..", "mcp", "testdata", "call-dividend-calendar.sse"))
	if err != nil {
		t.Fatal(err)
	}
	result := toolText(t, sse)
	m, err := provenance.NewManifest(provenance.Record{Tool: "dividend_calendar", Result: result})
	if err != nil {
		t.Fatal(err)
	}

	if got := provenance.CheckProse(reply.Message.Content, m); len(got) != 0 {
		t.Errorf("findings %v in the captured answer:\n%s", got, reply.Message.Content)
	}
}

// toolText extracts the tool's own output from a captured MCP event stream.
func toolText(t *testing.T, sse []byte) string {
	t.Helper()
	for line := range strings.SplitSeq(string(sse), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var msg struct {
				Result struct {
					Content []struct{ Text string } `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(data), &msg); err != nil {
				t.Fatal(err)
			}
			return msg.Result.Content[0].Text
		}
	}
	t.Fatal("no data line in the event stream")
	return ""
}

func TestNoFigures(t *testing.T) {
	tests := []struct {
		text string
		want []string
	}{
		// The example docs/rendering.md gives of prose a post may contain...
		{"Several consumer-staples names go ex-dividend in the same week for the first time this quarter.", nil},
		// ...a count in words, which nothing in a post's prose can verify...
		{"Three consumer-staples names go ex-dividend in the same week.", []string{"Three"}},
		// ...and of prose it may not.
		{"Yields rose about 40 basis points.", []string{"40"}},
		{"Microsoft goes ex-dividend on Oct 8.", []string{"Oct 8"}},
		{"The 2026 season.", []string{"2026"}},
	}
	for _, tt := range tests {
		if got := texts(provenance.NoFigures(tt.text)); !slices.Equal(got, tt.want) {
			t.Errorf("NoFigures(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestCheckData(t *testing.T) {
	m := manifest(t,
		`{"stocks":[{"symbol":"MSFT","ex_dividend_date":"2026-10-08"},{"symbol":"O","ex_dividend_date":"2026-10-06"}]}`,
		`{"symbol":"MSFT","dividend":{"amount":0.83,"currency":"USD","yield_pct":0.71}}`,
	)

	type exDividend struct {
		Symbol   string  `json:"symbol"`
		ExDate   string  `json:"ex_date"`
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
		YieldPct float64 `json:"yield_pct"`
	}
	type data struct {
		ExDividends []exDividend `json:"ex_dividends"`
	}

	tests := []struct {
		name string
		data any
		want []string
	}{
		{"everything traced", data{[]exDividend{{"MSFT", "2026-10-08", 0.83, "USD", 0.71}}}, nil},
		{"a rounded yield", data{[]exDividend{{"MSFT", "2026-10-08", 0.83, "USD", 0.7}}},
			[]string{"ex_dividends[0].yield_pct = 0.7"}},
		{"an invented ticker", data{[]exDividend{{"MSFTX", "2026-10-08", 0.83, "USD", 0.71}}},
			[]string{"ex_dividends[0].symbol = MSFTX"}},
		{"a date no tool returned, second row", data{[]exDividend{
			{"MSFT", "2026-10-08", 0.83, "USD", 0.71},
			{"O", "2026-10-07", 0.83, "USD", 0.71},
		}}, []string{"ex_dividends[1].ex_date = 2026-10-07"}},
		// A map works the same as a struct, and findings come out in key
		// order, not Go's random map order.
		{"a map, two findings", map[string]any{"zeta": 9.99, "alpha": "nope", "symbol": "O"},
			[]string{"alpha = nope", "zeta = 9.99"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings, err := provenance.CheckData(tt.data, m)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range findings {
				got = append(got, f.String())
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("CheckData reported %q, want %q", got, tt.want)
			}
		})
	}
}

func TestManifestRecordsWhereValuesCameFrom(t *testing.T) {
	m := calendar120(t)
	src := m.Date("2026-10-08")
	if len(src) != 2 {
		t.Fatalf("2026-10-08 found %d times, want 2 (JNJ and MSFT)", len(src))
	}
	if got := src[0].String(); !strings.HasPrefix(got, "dividend_calendar#0 $.stocks[") || !strings.HasSuffix(got, "].ex_dividend_date") {
		t.Errorf("source = %s, want the path into the calendar", got)
	}
	if len(m.Number(120)) != 1 {
		t.Errorf("120 found %v, want once, as the window", m.Number(120))
	}
}

func TestResultsMustBeJSON(t *testing.T) {
	_, err := provenance.NewManifest(provenance.Record{Tool: "calc", Result: "3.3 percent"})
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("err = %v, want a non-JSON result refused", err)
	}
}

// Figures from text added as a source, such as the question, are accounted
// for; ones it doesn't contain are still reported.
func TestTextAsASource(t *testing.T) {
	m := calendar120(t)
	m.AddText("question", "Which stocks go ex-dividend in the next six months?")
	m.AddText("today", "Today is 2026-10-07.")

	got := texts(provenance.CheckProse("Over the next six months (from 2026-10-07): about four months of data.", m))

	if !slices.Equal(got, []string{"four"}) {
		t.Errorf("findings = %q, want only the derived \"four\"", got)
	}
	if src := m.Number(6); len(src) != 1 || src[0].String() != `question: "six"` {
		t.Errorf("sources of 6 = %v, want the question", src)
	}
}

// A list's length is recorded with the list's path.
func TestListLengthsAreSources(t *testing.T) {
	m := manifest(t, `{"stocks":[{"symbol":"O"},{"symbol":"MSFT"}]}`)

	if src := m.Number(2); len(src) != 1 || src[0].Path != "len($.stocks)" {
		t.Errorf("sources of 2 = %v, want len($.stocks)", src)
	}
}
