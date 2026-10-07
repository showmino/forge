// Package output renders command results as table, json, yaml or name.
//
// JSON output of an API object is the API resource itself (protojson), so
// scripts see the same shape as the Connect JSON API. YAML is the same
// document re-encoded, with field order preserved.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// Format is an output format.
type Format string

// Supported formats.
const (
	Table Format = "table"
	JSON  Format = "json"
	YAML  Format = "yaml"
	Name  Format = "name"
)

// Formats lists the formats in the order shown in help text.
var Formats = []Format{Table, JSON, YAML, Name}

// Parse validates a format string. Empty means table.
func Parse(s string) (Format, error) {
	if s == "" {
		return Table, nil
	}
	for _, f := range Formats {
		if string(f) == s {
			return f, nil
		}
	}
	return "", fmt.Errorf("unknown output format %q (want table, json, yaml or name)", s)
}

// View is what a command hands to the printer: the same result in every shape.
type View struct {
	// Headers and Rows feed the table format.
	Headers []string
	Rows    [][]string
	// Names feed the name format, one per line.
	Names []string
	// Data feeds json and yaml: a proto.Message or any value encoding/json accepts.
	Data any
}

// Print writes v to w in format f.
func Print(w io.Writer, f Format, v View) error {
	switch f {
	case Table, "":
		return printTable(w, v)
	case Name:
		for _, n := range v.Names {
			if _, err := fmt.Fprintln(w, n); err != nil {
				return err
			}
		}
		return nil
	case JSON:
		data, err := marshalJSON(v.Data)
		if err != nil {
			return err
		}
		_, err = w.Write(append(data, '\n'))
		return err
	case YAML:
		data, err := marshalJSON(v.Data)
		if err != nil {
			return err
		}
		y, err := jsonToYAML(data)
		if err != nil {
			return err
		}
		_, err = w.Write(y)
		return err
	default:
		return fmt.Errorf("unknown output format %q", f)
	}
}

func printTable(w io.Writer, v View) error {
	tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
	if len(v.Headers) > 0 {
		h := make([]string, len(v.Headers))
		for i, s := range v.Headers {
			h[i] = strings.ToUpper(s)
		}
		fmt.Fprintln(tw, strings.Join(h, "\t"))
	}
	for _, row := range v.Rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = sanitize(c)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// sanitize keeps a cell on one line and stops tabs from breaking columns.
func sanitize(s string) string {
	if s == "" {
		return "-"
	}
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}

var protoJSON = protojson.MarshalOptions{Multiline: true, Indent: "  "}

func marshalJSON(v any) ([]byte, error) {
	if m, ok := v.(proto.Message); ok {
		raw, err := protoJSON.Marshal(m)
		if err != nil {
			return nil, err
		}
		// protojson output is deliberately unstable in whitespace; normalise it.
		var buf bytes.Buffer
		if err := json.Indent(&buf, compact(raw), "", "  "); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return json.MarshalIndent(v, "", "  ")
}

func compact(b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return b
	}
	return buf.Bytes()
}

// jsonToYAML re-encodes JSON as block-style YAML, keeping key order.
func jsonToYAML(data []byte) ([]byte, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	clearStyle(&node)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clearStyle(n *yaml.Node) {
	// The encoder still quotes strings that would read back as another type.
	n.Style = 0
	for _, c := range n.Content {
		clearStyle(c)
	}
}
