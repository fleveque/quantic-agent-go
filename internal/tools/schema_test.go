package tools_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/fleveque/quantic-agent/internal/tools"
)

func TestSchemaOf(t *testing.T) {
	type nested struct {
		Symbol string `json:"symbol"`
	}
	type args struct {
		Symbol   string   `json:"symbol" desc:"Ticker, e.g. MSFT"`
		Days     int      `json:"days,omitempty"`
		Ratio    float64  `json:"ratio"`
		Exact    bool     `json:"exact,omitempty"`
		Since    *string  `json:"since"`
		Tickers  []string `json:"tickers,omitempty"`
		Pair     nested   `json:"pair"`
		Untagged string
		Skipped  string `json:"-"`
		private  string
	}

	got, err := tools.SchemaOf(args{})
	if err != nil {
		t.Fatalf("SchemaOf: %v", err)
	}

	want := &tools.Schema{
		Type: "object",
		Properties: map[string]*tools.Schema{
			"symbol":   {Type: "string", Description: "Ticker, e.g. MSFT"},
			"days":     {Type: "integer"},
			"ratio":    {Type: "number"},
			"exact":    {Type: "boolean"},
			"since":    {Type: "string"},
			"tickers":  {Type: "array", Items: &tools.Schema{Type: "string"}},
			"pair":     {Type: "object", Properties: map[string]*tools.Schema{"symbol": {Type: "string"}}, Required: []string{"symbol"}},
			"Untagged": {Type: "string"},
		},
		// In field order. omitempty and pointers are optional.
		Required: []string{"symbol", "ratio", "pair", "Untagged"},
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("schema:\n%s\nwant:\n%s", g, w)
	}
}

func TestSchemaOfRejectsWhatItCannotDescribe(t *testing.T) {
	type withMap struct {
		M map[string]int `json:"m"`
	}
	for _, args := range []any{42, withMap{}, nil} {
		if _, err := tools.SchemaOf(args); err == nil {
			t.Errorf("SchemaOf(%T) succeeded, want an error", args)
		}
	}
}

// The server publishes its own schema for each tool. Ours is written
// independently, as a Go struct, so this test is what notices if the two
// drift apart: a renamed argument, a changed type.
func TestDividendCalendarMatchesTheServer(t *testing.T) {
	raw, err := os.ReadFile("../mcp/testdata/tools-list.json") // captured from quantic.finance
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name        string        `json:"name"`
				InputSchema *tools.Schema `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	var server *tools.Schema
	for _, tool := range list.Result.Tools {
		if tool.Name == tools.DividendCalendar.Name {
			server = tool.InputSchema
		}
	}
	if server == nil {
		t.Fatalf("the server doesn't list %s", tools.DividendCalendar.Name)
	}

	ours, err := tools.DividendCalendar.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if ours.Type != server.Type || len(ours.Properties) != len(server.Properties) {
		t.Fatalf("ours %+v, server %+v", ours, server)
	}
	for name, prop := range server.Properties {
		if mine, ok := ours.Properties[name]; !ok || mine.Type != prop.Type {
			t.Errorf("property %s: server says %s, ours %+v", name, prop.Type, mine)
		}
	}
	if !reflect.DeepEqual(ours.Required, server.Required) {
		t.Errorf("required = %v, server says %v", ours.Required, server.Required)
	}
}

func TestDecodeArgs(t *testing.T) {
	tests := []struct {
		raw     string
		want    any
		wantErr string
	}{
		{`{"days":10}`, tools.DividendCalendarArgs{Days: 10}, ""},
		{`{}`, tools.DividendCalendarArgs{}, ""},
		{``, tools.DividendCalendarArgs{}, ""},
		{`{"days":"ten"}`, nil, "cannot unmarshal string"},
		{`{"days":10,"sector":"Utilities"}`, nil, `unknown field "sector"`},
		{`{"days":2.5}`, nil, "cannot unmarshal number 2.5"},
		{`{"days":120}`, tools.DividendCalendarArgs{Days: 120}, ""},
		{`{"days":180}`, nil, "days is 180; it can be at most 120"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := tools.DividendCalendar.DecodeArgs(json.RawMessage(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeArgs: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestBoundsReachTheSchema(t *testing.T) {
	type args struct {
		Days  int     `json:"days" min:"1" max:"120"`
		Ratio float64 `json:"ratio" min:"0"`
	}
	s, err := tools.SchemaOf(args{})
	if err != nil {
		t.Fatal(err)
	}
	days, ratio := s.Properties["days"], s.Properties["ratio"]
	if days.Minimum == nil || *days.Minimum != 1 || days.Maximum == nil || *days.Maximum != 120 {
		t.Errorf("days bounds = %v, %v; want 1 and 120", days.Minimum, days.Maximum)
	}
	// A minimum of 0 is a real bound: it must be in the schema, not dropped.
	if ratio.Minimum == nil || *ratio.Minimum != 0 || ratio.Maximum != nil {
		t.Errorf("ratio bounds = %v, %v; want 0 and none", ratio.Minimum, ratio.Maximum)
	}
	out, _ := json.Marshal(s)
	if !strings.Contains(string(out), `"minimum":0`) {
		t.Errorf("schema JSON %s lost the zero minimum", out)
	}
}

func TestBoundsOnlyOnNumbers(t *testing.T) {
	type args struct {
		Symbol string `json:"symbol" max:"5"`
	}
	if _, err := tools.SchemaOf(args{}); err == nil || !strings.Contains(err.Error(), "needs a number") {
		t.Errorf("err = %v, want a max tag on a string refused", err)
	}
}
