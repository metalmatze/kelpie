package diff

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/itchyny/gojq"

	"github.com/metalmatze/kelpie/internal/core"
)

// Mapper turns a command's raw JSON output into observations.
type Mapper struct {
	code  *gojq.Code
	slurp bool
}

func NewMapper(expr string, slurp bool) (*Mapper, error) {
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("parse map: %w", err)
	}
	code, err := gojq.Compile(q, gojq.WithVariables([]string{"$parent", "$subject"}))
	if err != nil {
		return nil, fmt.Errorf("compile map: %w", err)
	}
	return &Mapper{code: code, slurp: slurp}, nil
}

// Map runs the map over every JSON document in raw. gh api --paginate prints
// one document per page; with slurp they become one array, as jq -s would.
//
// For a for_each source, parent is the observation it runs for: the map sees
// its fields as $parent and its subject as $subject, and whatever the map
// leaves out (subject, url, bind, fetch) is inherited, so binding works
// without repeating the parent's keys.
func (m *Mapper) Map(raw []byte, parent *core.Observation) ([]core.Observation, error) {
	docs, err := decodeAll(raw)
	if err != nil {
		return nil, err
	}
	if m.slurp {
		docs = []any{docs}
	}
	var pf map[string]any
	var ps string
	if parent != nil {
		pf, ps = parent.Fields, string(parent.Subject)
	}
	var obs []core.Observation
	for _, doc := range docs {
		iter := m.code.Run(doc, pf, ps)
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if err, ok := v.(error); ok {
				if haltErr, ok := errors.AsType[*gojq.HaltError](err); ok && haltErr.Value() == nil {
					break
				}
				return nil, fmt.Errorf("map: %w", err)
			}
			o, err := toObservation(v)
			if err != nil {
				return nil, err
			}
			if parent != nil {
				o.Subject = cmp.Or(o.Subject, parent.Subject)
				o.URL = cmp.Or(o.URL, parent.URL)
				if o.Bind == nil {
					o.Bind = parent.Bind
				}
				if o.Fetch == nil {
					o.Fetch = parent.Fetch
				}
			}
			if o.Subject == "" {
				return nil, fmt.Errorf("map output for %v has no subject", o.Fields)
			}
			obs = append(obs, o)
		}
	}
	return obs, nil
}

// toObservation round-trips through JSON so field values have the same types
// (float64, not int) whether they came from the map or from a stored snapshot.
func toObservation(v any) (core.Observation, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return core.Observation{}, err
	}
	var o core.Observation
	if err := json.Unmarshal(b, &o); err != nil {
		return core.Observation{}, fmt.Errorf("map output %s is not an observation: %w", truncate(b, 200), err)
	}
	return o, nil
}

func decodeAll(raw []byte) ([]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var docs []any
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode output: %w", err)
		}
		docs = append(docs, v)
	}
}

// Expr is a jq expression evaluated against an observation's new fields, with
// the previous fields bound to $old.
type Expr struct{ code *gojq.Code }

func NewExpr(expr string) (*Expr, error) {
	if expr == "" {
		return nil, nil
	}
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", expr, err)
	}
	code, err := gojq.Compile(q, gojq.WithVariables([]string{"$old"}))
	if err != nil {
		return nil, fmt.Errorf("compile %q: %w", expr, err)
	}
	return &Expr{code: code}, nil
}

// Eval returns the expression's first output.
func (e *Expr) Eval(fields, old map[string]any) (any, error) {
	v, ok := e.code.Run(fields, old).Next()
	if !ok {
		return nil, errors.New("no output")
	}
	if err, ok := v.(error); ok {
		return nil, err
	}
	return v, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
