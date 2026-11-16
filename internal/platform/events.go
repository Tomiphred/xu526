package platform

import (
	"context"
	"fmt"
	"net"
	"os"
	goruntime "runtime"
	"sync"
	"time"

	"xu526/internal/runtime"
)

// Event is one entry in the activity feed.
type Event struct {
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	App  string    `json:"app"`
	Kind string    `json:"kind"` // deploy, ready, stopped, crashed, error, delete
	Msg  string    `json:"msg"`
}

type eventLog struct {
	mu  sync.Mutex
	seq uint64
	buf []Event
}

const maxEvents = 500

func (p *Platform) event(app, kind, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	p.log.Printf("app %s: %s", app, msg)
	if p.logs != nil {
		p.logs.Write("xu", "platform", kind, app+": "+msg)
	}
	p.events.mu.Lock()
	p.events.seq++
	p.events.buf = append(p.events.buf, Event{Seq: p.events.seq, Time: time.Now(), App: app, Kind: kind, Msg: msg})
	if len(p.events.buf) > maxEvents {
		p.events.buf = append([]Event(nil), p.events.buf[len(p.events.buf)-maxEvents:]...)
	}
	p.events.mu.Unlock()
}

// Events returns events newer than since, oldest first.
func (p *Platform) Events(since uint64) []Event {
	p.events.mu.Lock()
	defer p.events.mu.Unlock()
	out := []Event{}
	for _, e := range p.events.buf {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	return out
}

// InstanceLogs is the recent output of one instance.
type InstanceLogs struct {
	ID   string `json:"id"`
	Logs string `json:"logs"`
}

// Logs returns the last tail lines of each running instance of an app.
func (p *Platform) Logs(ctx context.Context, name string, tail int) ([]InstanceLogs, error) {
	a := p.get(name)
	if a == nil {
		return nil, fmt.Errorf("no app %q", name)
	}
	lr, ok := a.rt.(runtime.LogReader)
	if !ok {
		return nil, fmt.Errorf("the %s runtime does not keep logs", a.rt.Name())
	}
	a.mu.Lock()
	insts := make([]*instance, len(a.instances))
	copy(insts, a.instances)
	a.mu.Unlock()

	out := make([]InstanceLogs, 0, len(insts))
	for _, in := range insts {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		text, err := lr.Logs(c, in.Instance, tail)
		cancel()
		if err != nil {
			text = "(could not read logs: " + err.Error() + ")"
		}
		out = append(out, InstanceLogs{ID: in.ID, Logs: text})
	}
	return out, nil
}

// NodeStatus describes the physical server xud runs on.
type NodeStatus struct {
	Hostname   string   `json:"hostname"`
	CPUs       int      `json:"cpus"`
	Load1      float64  `json:"load1"`
	Load5      float64  `json:"load5"`
	MemTotalMB int64    `json:"mem_total_mb"`
	MemAvailMB int64    `json:"mem_avail_mb"`
	Uptime     string   `json:"daemon_uptime"`
	Domain     string   `json:"domain"`
	PublicIP   string   `json:"public_ip"`
	PrivateIP  bool     `json:"private_ip"`
	Relay      bool     `json:"relay"` // the default domain only works on the local network
	Runtimes   []string `json:"runtimes"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	EngineArch string   `json:"engine_arch,omitempty"` // what images must be built for`
	Apps       int      `json:"apps"`
	Instances  int      `json:"instances"`
}

// Node reports host resources and platform totals.
func (p *Platform) Node() NodeStatus {
	h, _ := os.Hostname()
	n := NodeStatus{
		Hostname: h, CPUs: goruntime.NumCPU(), Domain: p.cfg.Domain, PublicIP: p.cfg.PublicIP,
		PrivateIP: p.cfg.PublicIP != "" && isPrivateIP(p.cfg.PublicIP), Relay: p.relay != nil,
		Uptime: time.Since(p.started).Round(time.Second).String(),
	}
	n.Load1, n.Load5, n.MemTotalMB, n.MemAvailMB = hostStats()
	n.OS, n.Arch = goruntime.GOOS, goruntime.GOARCH
	if d, ok := p.cfg.Docker.(interface{ Arch() string }); ok {
		n.EngineArch = d.Arch()
	}
	for _, rt := range []runtime.Runtime{p.cfg.Docker, p.cfg.Process} {
		if rt != nil {
			n.Runtimes = append(n.Runtimes, rt.Name())
		}
	}
	for _, s := range p.List() {
		n.Apps++
		n.Instances += s.Running
	}
	return n
}

func isPrivateIP(s string) bool {
	ip := net.ParseIP(s)
	return ip == nil || ip.IsPrivate() || ip.IsLoopback()
}
