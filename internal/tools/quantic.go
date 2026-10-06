package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// Tool is a tool the research loop may offer the model: a name the model
// calls it by, a description written for the model, and the struct its
// arguments must decode into. The struct is the schema (see SchemaOf).
type Tool struct {
	Name        string
	Description string
	Args        any // the zero value of the argument struct
}

// Schema is the tool's argument schema, derived from Args.
func (t Tool) Schema() (*Schema, error) {
	return SchemaOf(t.Args)
}

// DecodeArgs checks the arguments a model produced against the tool's
// argument struct and returns them as a value of that struct. It is strict on
// purpose: an argument the tool doesn't take, or a value of the wrong type,
// is an error to send back to the model, not something to drop quietly.
// Absent arguments ("{}" or nothing) take the struct's zero values.
func (t Tool) DecodeArgs(raw json.RawMessage) (any, error) {
	v := reflect.New(reflect.TypeOf(t.Args)) // a *Args, so the decoder can fill it in
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v.Interface()); err != nil {
			return nil, fmt.Errorf("tools: %s arguments: %w", t.Name, err)
		}
	}
	return v.Elem().Interface(), nil
}

// DividendCalendarArgs are dividend_calendar's arguments.
type DividendCalendarArgs struct {
	Days int `json:"days,omitempty" desc:"How many days ahead to look, counting from today. Defaults to 45; at most 120."`
}

// DividendCalendar lists upcoming ex-dividend dates. It is one of Quantic's
// public reference tools: the same answer for every caller, no account needed.
//
// The description is the agent's own, not the server's. The server's ends
// with buying advice ("buy before the ex-date..."), and the agent's output is
// informational, never advisory (design N5). What the model is told a tool is
// for shapes what it writes.
var DividendCalendar = Tool{
	Name: "dividend_calendar",
	Description: "List the companies going ex-dividend soon, soonest first. " +
		"Each entry has the company name, symbol, sector, ex-dividend date (YYYY-MM-DD) " +
		"and how often it pays. The list starts today and covers the given number of days.",
	Args: DividendCalendarArgs{},
}
