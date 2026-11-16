package telemetry

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// --- tiny protobuf encoder for building OTLP test messages ---
func tag(f, w int) []byte { return binary.AppendUvarint(nil, uint64(f<<3|w)) }
func lenField(f int, b []byte) []byte {
	return append(append(tag(f, 2), binary.AppendUvarint(nil, uint64(len(b)))...), b...)
}
func varField(f int, v uint64) []byte { return append(tag(f, 0), binary.AppendUvarint(nil, v)...) }
func fixed64(f int, v uint64) []byte {
	return append(tag(f, 1), binary.LittleEndian.AppendUint64(nil, v)...)
}
func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func kvStr(k, v string) []byte {
	return cat(lenField(1, []byte(k)), lenField(2, lenField(1, []byte(v))))
}

func TestDecodeOTLPProto(t *testing.T) {
	tid, _ := hex.DecodeString("0af7651916cd43dd8448eb211c80319c")
	sid, _ := hex.DecodeString("b7ad6b7169203331")
	pid, _ := hex.DecodeString("00f067aa0ba902b7")
	span := cat(lenField(1, tid), lenField(2, sid), lenField(4, pid), lenField(5, []byte("SELECT users")),
		varField(6, 3), fixed64(7, 1_000_000_000), fixed64(8, 1_250_000_000),
		lenField(9, kvStr("db.statement", "SELECT * FROM users WHERE id = ?")),
		lenField(9, cat(lenField(1, []byte("rows")), lenField(2, varField(3, 42)))),
		lenField(15, cat(lenField(2, []byte("timeout")), varField(3, 2))))
	req := lenField(1, cat(lenField(1, lenField(1, kvStr("service.name", "api"))), lenField(2, lenField(2, span))))
	spans, err := DecodeOTLPProto(req)
	if err != nil || len(spans) != 1 {
		t.Fatalf("%v %v", spans, err)
	}
	s := spans[0]
	if s.TraceID != "0af7651916cd43dd8448eb211c80319c" || s.ParentID != "00f067aa0ba902b7" || s.Name != "SELECT users" ||
		s.Service != "api" || s.Kind != "client" || s.Duration != 250_000_000 || !s.Error || s.Message != "timeout" ||
		s.Attrs["db.statement"] == "" || s.Attrs["rows"] != "42" {
		t.Fatalf("%+v", s)
	}
}

func TestDecodeOTLPJSON(t *testing.T) {
	js := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"web"}}]},
	"scopeSpans":[{"spans":[{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331","name":"GET /",
	"kind":2,"startTimeUnixNano":"1000","endTimeUnixNano":"5000","attributes":[{"key":"http.status_code","value":{"intValue":"200"}}],
	"status":{"code":2}}]}]}]}`
	spans, err := DecodeOTLPJSON([]byte(js))
	if err != nil || len(spans) != 1 || spans[0].Duration != 4000 || spans[0].Kind != "server" || !spans[0].Error ||
		spans[0].Attrs["http.status_code"] != "200" || spans[0].Service != "web" {
		t.Fatalf("%+v %v", spans, err)
	}
}

//go:noinline
func burn(d time.Duration) float64 {
	x := 0.0
	for end := time.Now().Add(d); time.Now().Before(end); {
		for i := 0; i < 1000; i++ {
			x += math.Sqrt(float64(i))
		}
	}
	return x
}

func TestParseRealPprof(t *testing.T) {
	var cpu bytes.Buffer
	if err := pprof.StartCPUProfile(&cpu); err != nil {
		t.Skip("cpu profiling unavailable:", err)
	}
	burn(300 * time.Millisecond)
	pprof.StopCPUProfile()
	profs, err := ParsePprof(cpu.Bytes())
	if err != nil || len(profs) != 1 || profs[0].Type != "cpu" || profs[0].Unit != "nanoseconds" {
		t.Fatalf("%+v %v", profs, err)
	}
	found := false
	for stack := range profs[0].Stacks {
		if strings.Contains(stack, "telemetry.burn") && strings.Contains(stack, "telemetry.TestParseRealPprof;") {
			found = true
		}
	}
	if !found {
		t.Fatalf("burn not found in %d stacks", len(profs[0].Stacks))
	}

	var heap bytes.Buffer
	_ = make([]byte, 8<<20)
	pprof.Lookup("heap").WriteTo(&heap, 0)
	hp, err := ParsePprof(heap.Bytes())
	if err != nil || len(hp) != 4 || hp[1].Type != "alloc_space" || hp[3].Type != "inuse_space" {
		t.Fatalf("heap: %+v %v", hp, err)
	}
}

func TestParseFoldedAndFlame(t *testing.T) {
	st, _ := ParseFolded(strings.NewReader("main;handle;db.query 70\nmain;handle;render 20\nmain;gc 10\nbad line\n"))
	ps := NewProfileStore(time.Hour)
	ps.Add("api", "cpu", "samples", "pyroscope", time.Now(), st)
	f := ps.Flame("api", "cpu", time.Now().Add(-time.Minute), time.Now())
	if f.Total != 100 || f.Root.Children[0].Name != "main" || f.Root.Children[0].Total != 100 {
		t.Fatalf("%+v", f.Root)
	}
	if f.Top[0].Name != "db.query" || f.Top[0].Self != 70 {
		t.Fatalf("top: %+v", f.Top)
	}
	for _, x := range f.Top {
		if x.Name == "handle" && (x.Self != 0 || x.Total != 90) {
			t.Fatalf("handle: %+v", x)
		}
	}
}

func TestTracesEndpointsAndLateSpans(t *testing.T) {
	s := NewTraceStore(100)
	now := time.Now().UnixNano()
	for i := 0; i < 50; i++ {
		d := int64(5 * time.Millisecond)
		if i%10 == 0 {
			d = int64(400 * time.Millisecond)
		}
		s.AddRequest(&Trace{ID: hex.EncodeToString([]byte{byte(i), 1}), App: "api", Method: "GET", Path: "/users/" + string(rune('0'+i%10)),
			Route: RouteOf("/users/7"), Start: now, Duration: d, Status: 200})
	}
	s.AddRequest(&Trace{ID: "slow1", App: "api", Method: "POST", Route: "/checkout", Start: now, Duration: int64(2 * time.Second), Status: 500, Error: true})
	eps := s.Endpoints("api", 60)
	if len(eps) != 2 || eps[0].Route != "/users/:id" || eps[0].Count != 50 || eps[1].ErrorRate != 1 || eps[0].P99 < 300 {
		t.Fatalf("%+v", eps)
	}
	if l := s.List("api", Query{Sort: "slowest", Limit: 3}); l[0].ID != "slow1" || len(l) != 3 {
		t.Fatalf("%+v", l)
	}
	// app spans arriving for a known trace attach to it
	s.AddSpans("api", []Span{{TraceID: "slow1", SpanID: "a1", ParentID: "r", Name: "SELECT orders", Start: now, Duration: 1e9}})
	if tr, _ := s.Get("slow1"); tr.SpanCount != 1 || tr.Spans[0].Name != "SELECT orders" {
		t.Fatalf("%+v", tr)
	}
	// spans for an unknown trace (a background job) become their own trace
	s.AddSpans("api", []Span{{TraceID: "job", SpanID: "j1", Name: "send-emails", Start: now, Duration: 3e9}})
	if tr, ok := s.Get("job"); !ok || tr.Route != "send-emails" || tr.Duration != 3e9 {
		t.Fatalf("%+v", tr)
	}
}

func TestRouteOf(t *testing.T) {
	for in, want := range map[string]string{
		"/users/42":                               "/users/:id",
		"/users/42/orders?page=2":                 "/users/:id/orders",
		"/o/7f3a9c2e-1b2c-4d5e-8f90-123456789abc": "/o/:id",
		"/static/app.js":                          "/static/app.js",
		"/":                                       "/",
		"/t/5f2b9c8d7e6a5b4c":                     "/t/:id",
	} {
		if got := RouteOf(in); got != want {
			t.Errorf("RouteOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodePushRequest(t *testing.T) {
	var cpu bytes.Buffer
	if err := pprof.StartCPUProfile(&cpu); err != nil {
		t.Skip(err)
	}
	burn(200 * time.Millisecond)
	pprof.StopCPUProfile()
	label := cat(lenField(1, []byte("__name__")), lenField(2, []byte("process_cpu")))
	sample := cat(lenField(1, cpu.Bytes()), lenField(2, []byte("id-1")))
	req := lenField(1, cat(lenField(1, label), lenField(2, sample)))
	raws, err := DecodePushRequest(req)
	if err != nil || len(raws) != 1 {
		t.Fatalf("%d %v", len(raws), err)
	}
	if profs, err := ParsePprof(raws[0]); err != nil || profs[0].Type != "cpu" {
		t.Fatalf("%+v %v", profs, err)
	}
}
