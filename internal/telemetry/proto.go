// Package telemetry decodes and stores traces and profiles.
//
// It speaks the standard formats so applications use ordinary libraries:
// OpenTelemetry (OTLP over HTTP, protobuf or JSON) for traces, and the
// Pyroscope ingest API (folded stacks or pprof) plus Go's pprof for profiles.
// The protobuf reader below is just enough for those messages, so the project
// keeps zero dependencies.
package telemetry

import (
	"encoding/binary"
	"errors"
	"math"
)

var errTruncated = errors.New("protobuf: truncated message")

// pb walks one protobuf message.
type pb struct {
	b []byte
}

// next returns the next field number, wire type and raw value. For varint
// and fixed fields the value is in v; for length-delimited fields in data.
func (m *pb) next() (field int, wire int, v uint64, data []byte, err error) {
	if len(m.b) == 0 {
		return 0, 0, 0, nil, nil
	}
	tag, n := binary.Uvarint(m.b)
	if n <= 0 {
		return 0, 0, 0, nil, errTruncated
	}
	m.b = m.b[n:]
	field, wire = int(tag>>3), int(tag&7)
	switch wire {
	case 0:
		v, n = binary.Uvarint(m.b)
		if n <= 0 {
			return 0, 0, 0, nil, errTruncated
		}
		m.b = m.b[n:]
	case 1:
		if len(m.b) < 8 {
			return 0, 0, 0, nil, errTruncated
		}
		v = binary.LittleEndian.Uint64(m.b)
		m.b = m.b[8:]
	case 2:
		l, n := binary.Uvarint(m.b)
		if n <= 0 || uint64(len(m.b)-n) < l {
			return 0, 0, 0, nil, errTruncated
		}
		data = m.b[n : n+int(l)]
		m.b = m.b[n+int(l):]
	case 5:
		if len(m.b) < 4 {
			return 0, 0, 0, nil, errTruncated
		}
		v = uint64(binary.LittleEndian.Uint32(m.b))
		m.b = m.b[4:]
	default:
		return 0, 0, 0, nil, errors.New("protobuf: unsupported wire type")
	}
	return field, wire, v, data, nil
}

func (m *pb) done() bool { return len(m.b) == 0 }

// packedUints reads a repeated integer field that may be packed or not.
func packedUints(wire int, v uint64, data []byte, out []uint64) []uint64 {
	if wire == 0 {
		return append(out, v)
	}
	for len(data) > 0 {
		x, n := binary.Uvarint(data)
		if n <= 0 {
			break
		}
		out = append(out, x)
		data = data[n:]
	}
	return out
}

func f64(v uint64) float64 { return math.Float64frombits(v) }
