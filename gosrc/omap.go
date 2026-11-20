// Ordered JSON object: preserves insertion order when marshaled, matching
// the TypeScript JSON.stringify order for GraphQL and MCP responses.
// encoding/json sorts map keys alphabetically, which would reorder fields
// the kit may compare; omap keeps the order the code sets.

package main

import (
	"encoding/json"
	"strings"
)

type omap struct {
	keys []string
	vals map[string]any
}

func newOmap() *omap {
	return &omap{vals: make(map[string]any)}
}

// om builds an omap from alternating key, value pairs.
func om(pairs ...any) *omap {
	o := newOmap()
	for i := 0; i+1 < len(pairs); i += 2 {
		if k, ok := pairs[i].(string); ok {
			o.set(k, pairs[i+1])
		}
	}
	return o
}

func (o *omap) set(k string, v any) *omap {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

func (o *omap) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *omap) MarshalJSON() ([]byte, error) {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		sb.Write(kb)
		sb.WriteByte(':')
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return []byte(sb.String()), nil
}

// jsonString marshals a value the way JSON.stringify would (ordered omaps
// keep their order); on error it yields "null".
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
