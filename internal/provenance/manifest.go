// Package provenance checks that every figure the agent writes came from a
// tool (design N1, §3.3).
//
// A run's successful tool calls are indexed into a Manifest: every number and
// every date their results contain, with where each one came from. Output is
// then checked against it, in one of three ways:
//
//   - CheckData walks structured data, the typed fields of a post, and
//     requires every number and date in it to be in the manifest. This is the
//     exact check the post format exists to make possible (docs/rendering.md).
//   - CheckProse extracts the figures from free text, such as a research
//     answer, and reports the ones no tool returned.
//   - NoFigures reports every figure in a post's prose, which must have none.
//
// Matching is exact. A figure the model rounded, converted or calculated is
// not the figure a tool returned, so it is reported: derived figures must come
// from the calculator tools, where they enter the manifest like any other
// result. Two things count as returned besides the values themselves: the
// length of every list in a result (so "nine stocks" is checked against a
// nine-item calendar), and the figures of text added with AddText, such as
// the question being answered.
package provenance

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

// Record is one successful tool call: which tool, and the text it returned.
// Calls that failed are not records: an error message is not data.
type Record struct {
	Tool   string
	Result string
}

// Source says where in the manifest a value was found: the call's position in
// the run, the tool, and the value's path inside that call's result. A source
// added with AddText has Call -1, its label as Tool, and the figure as written
// as Path.
type Source struct {
	Call int
	Tool string
	Path string
}

func (s Source) String() string {
	if s.Call < 0 { // added with AddText: no call, the text it came from
		return fmt.Sprintf("%s: %q", s.Tool, s.Path)
	}
	return fmt.Sprintf("%s#%d %s", s.Tool, s.Call, s.Path)
}

// Manifest indexes every number, date and string a run's tools returned.
type Manifest struct {
	numbers map[float64][]Source
	dates   map[string][]Source // YYYY-MM-DD
	strings map[string][]Source // every string value, dates included
}

// isoDate matches a whole string that is a calendar date.
var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// NewManifest indexes the results of a run's successful tool calls. Each
// result must be JSON, which is what Quantic's tools return; a result that
// isn't is an error, because its figures could not be accounted for.
func NewManifest(records ...Record) (*Manifest, error) {
	m := &Manifest{numbers: map[float64][]Source{}, dates: map[string][]Source{}, strings: map[string][]Source{}}
	for i, r := range records {
		var v any
		if err := json.Unmarshal([]byte(r.Result), &v); err != nil {
			return nil, fmt.Errorf("provenance: result of %s (call %d) is not JSON: %w", r.Tool, i, err)
		}
		m.index(v, Source{Call: i, Tool: r.Tool, Path: "$"})
	}
	return m, nil
}

// index walks a decoded JSON value and records its numbers and dates.
func (m *Manifest) index(v any, at Source) {
	switch v := v.(type) {
	case float64:
		m.numbers[v] = append(m.numbers[v], at)
	case string:
		m.strings[v] = append(m.strings[v], at)
		if isoDate.MatchString(v) {
			m.dates[v] = append(m.dates[v], at)
		}
	case []any:
		// How many items a list has is part of what the tool returned.
		length := Source{Call: at.Call, Tool: at.Tool, Path: "len(" + at.Path + ")"}
		m.numbers[float64(len(v))] = append(m.numbers[float64(len(v))], length)
		for i, item := range v {
			m.index(item, at.child(fmt.Sprintf("[%d]", i)))
		}
	case map[string]any:
		for key, item := range v {
			m.index(item, at.child("."+key))
		}
	}
	// Booleans and nulls carry no figures.
}

// AddText adds the figures written in text as a source, labelled source: the
// question being answered ("the next six months"), or today's date. They
// aren't data, but repeating them isn't inventing anything. Dates without a
// year are left out, since they name no particular day.
func (m *Manifest) AddText(source, text string) {
	for _, f := range figures(text) {
		at := Source{Call: -1, Tool: source, Path: f.text}
		switch {
		case f.kind == KindDate && f.year != 0:
			d := f.value()
			m.dates[d] = append(m.dates[d], at)
		case f.kind == KindYear:
			m.numbers[float64(f.year)] = append(m.numbers[float64(f.year)], at)
		case f.kind == KindNumber:
			m.numbers[f.number] = append(m.numbers[f.number], at)
		}
	}
}

func (s Source) child(step string) Source {
	s.Path += step
	return s
}

// Number reports where n appears in the manifest, if anywhere.
func (m *Manifest) Number(n float64) []Source { return m.numbers[n] }

// Date reports where a YYYY-MM-DD date appears in the manifest, if anywhere.
func (m *Manifest) Date(d string) []Source { return m.dates[d] }

// hasYear reports whether any manifest date falls in year. A bare year in
// prose ("in 2026") is accounted for by a date in that year: the year is part
// of a figure a tool returned.
func (m *Manifest) hasYear(year int) bool {
	prefix := strconv.Itoa(year) + "-"
	for d := range m.dates {
		if len(d) == 10 && d[:5] == prefix {
			return true
		}
	}
	return false
}

// datesOn returns the manifest dates with this month and day in any year,
// for prose that names a date without its year ("Oct 8").
func (m *Manifest) datesOn(month, day int) []string {
	suffix := fmt.Sprintf("-%02d-%02d", month, day)
	var out []string
	for d := range m.dates {
		if len(d) == 10 && d[4:] == suffix {
			out = append(out, d)
		}
	}
	sort.Strings(out) // map order is random; results shouldn't be
	return out
}
