package telemetry

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

// Span is one timed operation.
type Span struct {
	TraceID  string            `json:"trace_id"`
	SpanID   string            `json:"span_id"`
	ParentID string            `json:"parent_id,omitempty"`
	Name     string            `json:"name"`
	Service  string            `json:"service"`
	Kind     string            `json:"kind,omitempty"`
	Start    int64             `json:"start"`    // unix nanoseconds
	Duration int64             `json:"duration"` // nanoseconds
	Error    bool              `json:"error,omitempty"`
	Message  string            `json:"message,omitempty"`
	Attrs    map[string]string `json:"attrs,omitempty"`
}

var kinds = []string{"", "internal", "server", "client", "producer", "consumer"}

func kindName(k uint64) string {
	if int(k) < len(kinds) {
		return kinds[k]
	}
	return ""
}

// DecodeOTLPProto decodes an OTLP ExportTraceServiceRequest (protobuf).
func DecodeOTLPProto(b []byte) ([]Span, error) {
	var out []Span
	req := pb{b}
	for !req.done() {
		f, _, _, rs, err := req.next()
		if err != nil {
			return nil, err
		}
		if f != 1 { // resource_spans
			continue
		}
		service := ""
		var scopes [][]byte
		m := pb{rs}
		for !m.done() {
			f, _, _, d, err := m.next()
			if err != nil {
				return nil, err
			}
			switch f {
			case 1: // resource
				r := pb{d}
				for !r.done() {
					f, _, _, kv, err := r.next()
					if err != nil {
						return nil, err
					}
					if f == 1 {
						if k, v := decodeKV(kv); k == "service.name" {
							service = v
						}
					}
				}
			case 2:
				scopes = append(scopes, d)
			}
		}
		for _, sc := range scopes {
			s := pb{sc}
			for !s.done() {
				f, _, _, d, err := s.next()
				if err != nil {
					return nil, err
				}
				if f == 2 {
					sp, err := decodeSpan(d)
					if err != nil {
						return nil, err
					}
					sp.Service = service
					out = append(out, sp)
				}
			}
		}
	}
	return out, nil
}

func decodeSpan(b []byte) (Span, error) {
	var sp Span
	var start, end uint64
	m := pb{b}
	for !m.done() {
		f, _, v, d, err := m.next()
		if err != nil {
			return sp, err
		}
		switch f {
		case 1:
			sp.TraceID = hex.EncodeToString(d)
		case 2:
			sp.SpanID = hex.EncodeToString(d)
		case 4:
			sp.ParentID = hex.EncodeToString(d)
		case 5:
			sp.Name = string(d)
		case 6:
			sp.Kind = kindName(v)
		case 7:
			start = v
		case 8:
			end = v
		case 9:
			if k, val := decodeKV(d); k != "" {
				if sp.Attrs == nil {
					sp.Attrs = map[string]string{}
				}
				sp.Attrs[k] = val
			}
		case 15: // status
			st := pb{d}
			for !st.done() {
				f, _, v, d, err := st.next()
				if err != nil {
					return sp, err
				}
				if f == 2 {
					sp.Message = string(d)
				} else if f == 3 && v == 2 {
					sp.Error = true
				}
			}
		}
	}
	sp.Start = int64(start)
	if end > start {
		sp.Duration = int64(end - start)
	}
	return sp, nil
}

// decodeKV returns a KeyValue's key and its value as text.
func decodeKV(b []byte) (string, string) {
	var key, val string
	m := pb{b}
	for !m.done() {
		f, _, _, d, err := m.next()
		if err != nil {
			return "", ""
		}
		switch f {
		case 1:
			key = string(d)
		case 2:
			val = anyValue(d)
		}
	}
	return key, val
}

func anyValue(b []byte) string {
	m := pb{b}
	for !m.done() {
		f, _, v, d, err := m.next()
		if err != nil {
			return ""
		}
		switch f {
		case 1:
			return string(d)
		case 2:
			return strconv.FormatBool(v != 0)
		case 3:
			return strconv.FormatInt(int64(v), 10)
		case 4:
			return strconv.FormatFloat(f64(v), 'g', -1, 64)
		case 5, 6:
			return "[…]"
		case 7:
			return hex.EncodeToString(d)
		}
	}
	return ""
}

// DecodeOTLPJSON decodes the JSON encoding of the same request.
func DecodeOTLPJSON(b []byte) ([]Span, error) {
	type jv struct {
		StringValue *string         `json:"stringValue"`
		BoolValue   *bool           `json:"boolValue"`
		IntValue    json.RawMessage `json:"intValue"`
		DoubleValue *float64        `json:"doubleValue"`
	}
	type jkv struct {
		Key   string `json:"key"`
		Value jv     `json:"value"`
	}
	var req struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []jkv `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []struct {
					TraceID      string          `json:"traceId"`
					SpanID       string          `json:"spanId"`
					ParentSpanID string          `json:"parentSpanId"`
					Name         string          `json:"name"`
					Kind         json.RawMessage `json:"kind"`
					Start        json.RawMessage `json:"startTimeUnixNano"`
					End          json.RawMessage `json:"endTimeUnixNano"`
					Attributes   []jkv           `json:"attributes"`
					Status       struct {
						Code    json.RawMessage `json:"code"`
						Message string          `json:"message"`
					} `json:"status"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("invalid OTLP JSON: %w", err)
	}
	num := func(r json.RawMessage) uint64 {
		s := string(r)
		if len(s) > 1 && s[0] == '"' {
			s = s[1 : len(s)-1]
		}
		n, _ := strconv.ParseUint(s, 10, 64)
		return n
	}
	val := func(v jv) string {
		switch {
		case v.StringValue != nil:
			return *v.StringValue
		case v.BoolValue != nil:
			return strconv.FormatBool(*v.BoolValue)
		case len(v.IntValue) > 0:
			return fmt.Sprint(num(v.IntValue))
		case v.DoubleValue != nil:
			return strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
		}
		return ""
	}
	var out []Span
	for _, rs := range req.ResourceSpans {
		service := ""
		for _, a := range rs.Resource.Attributes {
			if a.Key == "service.name" {
				service = val(a.Value)
			}
		}
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				sp := Span{TraceID: s.TraceID, SpanID: s.SpanID, ParentID: s.ParentSpanID, Name: s.Name,
					Service: service, Kind: kindName(num(s.Kind)), Message: s.Status.Message}
				start, end := num(s.Start), num(s.End)
				sp.Start = int64(start)
				if end > start {
					sp.Duration = int64(end - start)
				}
				code := string(s.Status.Code)
				sp.Error = code == "2" || code == `"STATUS_CODE_ERROR"`
				for _, a := range s.Attributes {
					if sp.Attrs == nil {
						sp.Attrs = map[string]string{}
					}
					sp.Attrs[a.Key] = val(a.Value)
				}
				out = append(out, sp)
			}
		}
	}
	return out, nil
}
