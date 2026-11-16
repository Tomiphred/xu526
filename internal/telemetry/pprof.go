package telemetry

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strconv"
	"strings"
)

// Profile is a set of stacks ("root;caller;leaf") with a value each, for one
// profile type such as cpu or alloc_space.
type Profile struct {
	Type   string
	Unit   string
	Stacks map[string]int64
}

// ParsePprof decodes a (possibly gzipped) pprof profile into one Profile per
// sample type, e.g. a heap profile gives alloc_objects, alloc_space,
// inuse_objects and inuse_space.
func ParsePprof(b []byte) ([]Profile, error) {
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		if b, err = io.ReadAll(io.LimitReader(zr, 256<<20)); err != nil {
			return nil, err
		}
	}
	type valueType struct{ typ, unit int64 }
	type sample struct {
		locs   []uint64
		values []uint64
	}
	var (
		types    []valueType
		samples  []sample
		strs     []string
		funcName = map[uint64]int64{} // function id → name string index
		locFuncs = map[uint64][]uint64{}
	)
	m := pb{b}
	for !m.done() {
		f, _, _, d, err := m.next()
		if err != nil {
			return nil, err
		}
		switch f {
		case 1: // sample_type
			var vt valueType
			s := pb{d}
			for !s.done() {
				f, _, v, _, err := s.next()
				if err != nil {
					return nil, err
				}
				if f == 1 {
					vt.typ = int64(v)
				} else if f == 2 {
					vt.unit = int64(v)
				}
			}
			types = append(types, vt)
		case 2: // sample
			var sm sample
			s := pb{d}
			for !s.done() {
				f, w, v, d, err := s.next()
				if err != nil {
					return nil, err
				}
				if f == 1 {
					sm.locs = packedUints(w, v, d, sm.locs)
				} else if f == 2 {
					sm.values = packedUints(w, v, d, sm.values)
				}
			}
			samples = append(samples, sm)
		case 4: // location
			var id uint64
			var fns []uint64
			s := pb{d}
			for !s.done() {
				f, _, v, d, err := s.next()
				if err != nil {
					return nil, err
				}
				if f == 1 {
					id = v
				} else if f == 4 { // line: inlined frames first, caller last
					l := pb{d}
					for !l.done() {
						f, _, v, _, err := l.next()
						if err != nil {
							return nil, err
						}
						if f == 1 {
							fns = append(fns, v)
						}
					}
				}
			}
			locFuncs[id] = fns
		case 5: // function
			var id uint64
			var name int64
			s := pb{d}
			for !s.done() {
				f, _, v, _, err := s.next()
				if err != nil {
					return nil, err
				}
				if f == 1 {
					id = v
				} else if f == 2 {
					name = int64(v)
				}
			}
			funcName[id] = name
		case 6:
			strs = append(strs, string(d))
		}
	}
	str := func(i int64) string {
		if i >= 0 && int(i) < len(strs) {
			return strs[i]
		}
		return "?"
	}
	if len(types) == 0 {
		return nil, errors.New("pprof: no sample types")
	}
	out := make([]Profile, len(types))
	for i, t := range types {
		out[i] = Profile{Type: str(t.typ), Unit: str(t.unit), Stacks: map[string]int64{}}
	}
	if len(out) == 2 && out[0].Type == "samples" && out[1].Type == "cpu" {
		out = out[1:] // a CPU profile: keep nanoseconds, drop the sample count
		for i := range samples {
			if len(samples[i].values) == 2 {
				samples[i].values = samples[i].values[1:]
			}
		}
	}
	var frames []string
	for _, sm := range samples {
		frames = frames[:0]
		for i := len(sm.locs) - 1; i >= 0; i-- { // root first
			fns := locFuncs[sm.locs[i]]
			for j := len(fns) - 1; j >= 0; j-- {
				frames = append(frames, str(funcName[fns[j]]))
			}
		}
		if len(frames) == 0 {
			continue
		}
		key := strings.Join(frames, ";")
		for i, v := range sm.values {
			if i < len(out) && v != 0 {
				out[i].Stacks[key] += int64(v)
			}
		}
	}
	return out, nil
}

// ParseFolded reads "frame;frame;frame count" lines (Pyroscope's folded and
// lines formats, also what py-spy and many profilers emit).
func ParseFolded(r io.Reader) (map[string]int64, error) {
	stacks := map[string]int64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		i := strings.LastIndexByte(line, ' ')
		if i <= 0 {
			continue
		}
		n, err := strconv.ParseInt(line[i+1:], 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		stacks[line[:i]] += n
	}
	return stacks, sc.Err()
}

// DecodePushRequest extracts the pprof payloads from a Pyroscope PushRequest.
func DecodePushRequest(b []byte) ([][]byte, error) {
	var out [][]byte
	req := pb{b}
	for !req.done() {
		f, _, _, series, err := req.next()
		if err != nil {
			return nil, err
		}
		if f != 1 {
			continue
		}
		s := pb{series}
		for !s.done() {
			f, _, _, sample, err := s.next()
			if err != nil {
				return nil, err
			}
			if f != 2 {
				continue
			}
			sm := pb{sample}
			for !sm.done() {
				f, _, _, raw, err := sm.next()
				if err != nil {
					return nil, err
				}
				if f == 1 && len(raw) > 0 {
					out = append(out, raw)
				}
			}
		}
	}
	return out, nil
}
