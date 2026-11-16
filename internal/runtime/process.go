package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Process runs an app as a plain local process with PORT set. There is no
// isolation: use it for development and tests, never for tenants.
type Process struct {
	mu    sync.Mutex
	procs map[string]*proc
}

type proc struct {
	cmd   *exec.Cmd
	logs  *ring
	lines chan [2]string // stream, line
}

func NewProcess() *Process { return &Process{procs: map[string]*proc{}} }

func (p *Process) Name() string { return "process" }

func (p *Process) Start(ctx context.Context, s Spec) (*Instance, error) {
	if len(s.Command) == 0 {
		return nil, errors.New("process runtime needs a command")
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	logs := &ring{}
	lines := make(chan [2]string, 4096)
	cmd := exec.Command(s.Command[0], s.Command[1:]...)
	cmd.Env = append(os.Environ(), "PORT="+strconv.Itoa(port))
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = io.MultiWriter(os.Stdout, logs, &lineSplitter{stream: "stdout", out: lines})
	cmd.Stderr = io.MultiWriter(os.Stderr, logs, &lineSplitter{stream: "stderr", out: lines})
	cmd.SysProcAttr = groupAttr() // own process group, so Stop reaches its children
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	id := "proc-" + shortID()
	p.mu.Lock()
	p.procs[id] = &proc{cmd: cmd, logs: logs, lines: lines}
	p.mu.Unlock()

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		p.mu.Lock()
		delete(p.procs, id)
		p.mu.Unlock()
		close(exited)
	}()
	return &Instance{ID: id, Addr: "127.0.0.1:" + strconv.Itoa(port), Exited: exited}, nil
}

func (p *Process) Stop(ctx context.Context, inst *Instance) error {
	p.mu.Lock()
	pr := p.procs[inst.ID]
	p.mu.Unlock()
	if pr == nil {
		return nil
	}
	pid := pr.cmd.Process.Pid
	stopGroup(pid)
	select {
	case <-inst.Exited:
	case <-time.After(10 * time.Second):
		killGroup(pid)
		<-inst.Exited
	}
	return nil
}

func (p *Process) Logs(ctx context.Context, inst *Instance, tail int) (string, error) {
	p.mu.Lock()
	pr := p.procs[inst.ID]
	p.mu.Unlock()
	if pr == nil {
		return "", errors.New("instance is gone")
	}
	return pr.logs.tail(tail), nil
}

func (p *Process) Cleanup(ctx context.Context) error { return nil }

// ring keeps the last 64 KB of output.
type ring struct {
	mu sync.Mutex
	b  []byte
}

const ringMax = 64 << 10

func (r *ring) Write(b []byte) (int, error) {
	r.mu.Lock()
	r.b = append(r.b, b...)
	if len(r.b) > ringMax {
		r.b = append([]byte(nil), r.b[len(r.b)-ringMax:]...)
	}
	r.mu.Unlock()
	return len(b), nil
}

func (r *ring) tail(n int) string {
	r.mu.Lock()
	s := string(r.b)
	r.mu.Unlock()
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Follow delivers output lines until the process exits or ctx ends.
func (p *Process) Follow(ctx context.Context, inst *Instance, _ time.Time, fn func(stream, line string)) error {
	p.mu.Lock()
	pr := p.procs[inst.ID]
	p.mu.Unlock()
	if pr == nil {
		return errors.New("instance is gone")
	}
	for {
		select {
		case l := <-pr.lines:
			fn(l[0], l[1])
		case <-inst.Exited:
			for {
				select {
				case l := <-pr.lines:
					fn(l[0], l[1])
				default:
					return nil
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

type lineSplitter struct {
	stream string
	buf    []byte
	out    chan [2]string
}

func (l *lineSplitter) Write(b []byte) (int, error) {
	l.buf = append(l.buf, b...)
	for {
		i := strings.IndexByte(string(l.buf), '\n')
		if i < 0 {
			break
		}
		select {
		case l.out <- [2]string{l.stream, strings.TrimRight(string(l.buf[:i]), "\r")}:
		default: // nobody is reading fast enough: drop rather than block the app
		}
		l.buf = l.buf[i+1:]
	}
	return len(b), nil
}
