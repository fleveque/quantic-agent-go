package provenance

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind is what sort of figure a Finding is.
type Kind string

const (
	KindNumber Kind = "number"
	KindDate   Kind = "date"
	KindYear   Kind = "year"
)

// Finding is a figure in prose that no tool returned.
type Finding struct {
	Text   string // as written: "$0.83", "October 8, 2026", "180"
	Offset int    // byte offset in the checked text
	Kind   Kind
	Value  string // normalised: "0.83", "2026-10-08", "--10-08" for a date with no year
}

func (f Finding) String() string { return fmt.Sprintf("%q (%s %s)", f.Text, f.Kind, f.Value) }

// figure is one figure found in prose, before it is checked.
type figure struct {
	text   string
	offset int
	kind   Kind
	number float64
	year   int // dates: 0 when the text names no year
	month  int
	day    int
}

// value is the figure in normal form, for reporting.
func (f figure) value() string {
	switch f.kind {
	case KindDate:
		if f.year == 0 {
			return fmt.Sprintf("--%02d-%02d", f.month, f.day)
		}
		return fmt.Sprintf("%04d-%02d-%02d", f.year, f.month, f.day)
	case KindYear:
		return strconv.Itoa(f.year)
	}
	return strconv.FormatFloat(f.number, 'f', -1, 64)
}

// CheckProse reports the figures in text that the manifest doesn't account
// for. An empty result means every figure traces to a tool result.
//
// It recognises numbers (with thousands separators, a currency symbol or a
// percent sign), dates (2026-10-08, "Oct 8", "October 8, 2026", "8 October")
// and bare years. Figures written as words ("five companies") are not seen;
// the post format makes that moot, since its prose may hold no figures at all
// (see NoFigures).
func CheckProse(text string, m *Manifest) []Finding {
	var out []Finding
	for _, f := range figures(text) {
		if !m.accounts(f) {
			out = append(out, f.finding())
		}
	}
	return out
}

// NoFigures reports every figure in text. A post's prose must have none: its
// figures belong in the typed data, where CheckData verifies them exactly
// (docs/rendering.md).
func NoFigures(text string) []Finding {
	var out []Finding
	for _, f := range figures(text) {
		out = append(out, f.finding())
	}
	return out
}

func (f figure) finding() Finding {
	return Finding{Text: f.text, Offset: f.offset, Kind: f.kind, Value: f.value()}
}

// accounts reports whether the manifest contains the figure.
func (m *Manifest) accounts(f figure) bool {
	switch f.kind {
	case KindDate:
		if f.year == 0 {
			return len(m.datesOn(f.month, f.day)) > 0
		}
		return len(m.Date(f.value())) > 0
	case KindYear:
		return len(m.Number(float64(f.year))) > 0 || m.hasYear(f.year)
	}
	return len(m.Number(f.number)) > 0
}

const monthNames = `january|february|march|april|may|june|july|august|september|october|november|december|` +
	`jan|feb|mar|apr|jun|jul|aug|sept|sep|oct|nov|dec`

var (
	isoInProse = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	monthDay   = regexp.MustCompile(`(?i)\b(` + monthNames + `)\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b(?:,?\s+(\d{4})\b)?`)
	dayMonth   = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(` + monthNames + `)\b\.?(?:,?\s+(\d{4})\b)?`)

	// A number: an optional sign and currency symbol, digits with or without
	// thousands separators, an optional decimal part and percent sign.
	number = regexp.MustCompile(`-?[$€£]?(?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?%?`)
)

// figures finds every figure in text, in order. Dates are found first and
// blanked out, so the "8" in "Oct 8" isn't also read as a number.
func figures(text string) []figure {
	var out []figure
	masked := []byte(text)
	blank := func(start, end int) {
		for i := start; i < end; i++ {
			masked[i] = ' '
		}
	}

	for _, loc := range isoInProse.FindAllStringSubmatchIndex(text, -1) {
		y, mo, d := atoi(text, loc, 2), atoi(text, loc, 4), atoi(text, loc, 6)
		out = append(out, figure{text: text[loc[0]:loc[1]], offset: loc[0], kind: KindDate, year: y, month: mo, day: d})
		blank(loc[0], loc[1])
	}
	for _, re := range []*regexp.Regexp{monthDay, dayMonth} {
		for _, loc := range re.FindAllStringSubmatchIndex(string(masked), -1) {
			s := string(masked)
			var mo, d int
			if re == monthDay {
				mo, d = monthNumber(s[loc[2]:loc[3]]), atoi(s, loc, 4)
			} else {
				d, mo = atoi(s, loc, 2), monthNumber(s[loc[4]:loc[5]])
			}
			y := 0
			if loc[6] >= 0 {
				y = atoi(s, loc, 6)
			}
			out = append(out, figure{text: text[loc[0]:loc[1]], offset: loc[0], kind: KindDate, year: y, month: mo, day: d})
			blank(loc[0], loc[1])
		}
	}

	s := string(masked)
	for _, loc := range number.FindAllStringIndex(s, -1) {
		start, end := loc[0], loc[1]
		if partOfWord(s, start, end) || listMarker(s, start, end) {
			continue
		}
		raw := s[start:end]
		clean := strings.NewReplacer(",", "", "$", "", "€", "", "£", "", "%", "").Replace(raw)
		n, err := strconv.ParseFloat(clean, 64)
		if err != nil {
			continue
		}
		f := figure{text: raw, offset: start, kind: KindNumber, number: n}
		// A bare four-digit integer between 1900 and 2100 reads as a year.
		if clean == raw && n == float64(int(n)) && n >= 1900 && n <= 2100 && len(raw) == 4 {
			f.kind, f.year = KindYear, int(n)
		}
		out = append(out, f)
	}

	slices.SortFunc(out, func(a, b figure) int { return cmp.Compare(a.offset, b.offset) })
	return out
}

// partOfWord reports whether the digits at s[start:end] belong to a word
// such as "Q3", "W38", "qwen3.5" or "3M", rather than standing alone.
func partOfWord(s string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:start])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' {
			return true
		}
	}
	if end < len(s) {
		r, _ := utf8.DecodeRuneInString(s[end:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			return true
		}
	}
	return false
}

// listMarker reports whether the number is a list position at the start of a
// line ("1. ", "2) "): a position, not a claim.
func listMarker(s string, start, end int) bool {
	lineStart := strings.LastIndexByte(s[:start], '\n') + 1
	if strings.TrimSpace(s[lineStart:start]) != "" {
		return false
	}
	return end < len(s) && (s[end] == '.' || s[end] == ')') && end+1 < len(s) && s[end+1] == ' '
}

func atoi(s string, loc []int, group int) int {
	n, _ := strconv.Atoi(s[loc[group]:loc[group+1]])
	return n
}

func monthNumber(name string) int {
	name = strings.ToLower(name)
	for i, m := range []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"} {
		if strings.HasPrefix(name, m) {
			return i + 1
		}
	}
	return 0
}
