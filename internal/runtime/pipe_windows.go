package runtime

// A named-pipe connection (Docker Desktop listens on \\.\pipe\docker_engine)
// using overlapped I/O, so a read and a write can be in flight at once, as
// an HTTP connection needs. Go's os.File reads pipes synchronously under a
// lock that writes also take, which would deadlock here.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procWaitNamedPipeW      = kernel32.NewProc("WaitNamedPipeW")
)

const (
	errorPipeBusy         = syscall.Errno(231)
	errorBrokenPipe       = syscall.Errno(109)
	errorPipeNotConnected = syscall.Errno(233)
	errorNoData           = syscall.Errno(232)
)

func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	for {
		h, err := syscall.CreateFile(p16, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
			syscall.OPEN_EXISTING, syscall.FILE_FLAG_OVERLAPPED, 0)
		if err == nil {
			return &pipeConn{h: h, path: path}, nil
		}
		if err != errorPipeBusy {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		// Every instance is busy: wait a little for one, until ctx ends.
		procWaitNamedPipeW.Call(uintptr(unsafe.Pointer(p16)), 250)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

type pipeConn struct {
	h    syscall.Handle
	path string

	mu       sync.Mutex
	closed   bool
	inflight sync.WaitGroup
	rdl, wdl time.Time
}

func (c *pipeConn) deadline(write bool) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if write {
		return c.wdl, c.closed
	}
	return c.rdl, c.closed
}

// do runs one overlapped read or write and waits for it, giving up when the
// deadline passes or the connection closes.
func (c *pipeConn) do(b []byte, write bool) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.inflight.Add(1)
	c.mu.Unlock()
	defer c.inflight.Done()

	ev, _, e := procCreateEventW.Call(0, 1, 0, 0) // manual reset, not signalled
	if ev == 0 {
		return 0, e
	}
	event := syscall.Handle(ev)
	defer syscall.CloseHandle(event)
	o := &syscall.Overlapped{HEvent: event}
	var n uint32
	var err error
	if write {
		err = syscall.WriteFile(c.h, b, &n, o)
	} else {
		err = syscall.ReadFile(c.h, b, &n, o)
	}
	if err != nil && err != syscall.ERROR_IO_PENDING {
		return 0, c.mapErr(err, write)
	}
	var stop error
	for {
		r, _ := syscall.WaitForSingleObject(event, 100)
		if r == syscall.WAIT_OBJECT_0 {
			break
		}
		dl, closed := c.deadline(write)
		switch {
		case closed:
			stop = net.ErrClosed
		case !dl.IsZero() && time.Now().After(dl):
			stop = os.ErrDeadlineExceeded
		default:
			continue
		}
		syscall.CancelIoEx(c.h, o)
		syscall.WaitForSingleObject(event, syscall.INFINITE) // the cancelled I/O finishes
		break
	}
	r1, _, e := procGetOverlappedResult.Call(uintptr(c.h), uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&n)), 0)
	if stop != nil && (r1 == 0 || n == 0) {
		return int(n), stop
	}
	if r1 == 0 {
		return int(n), c.mapErr(e, write)
	}
	return int(n), nil
}

func (c *pipeConn) mapErr(err error, write bool) error {
	var en syscall.Errno
	if errors.As(err, &en) {
		switch en {
		case errorBrokenPipe, errorPipeNotConnected, errorNoData:
			if write {
				return syscall.EPIPE
			}
			return io.EOF
		case syscall.ERROR_OPERATION_ABORTED:
			return net.ErrClosed
		}
	}
	return err
}

func (c *pipeConn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	return c.do(b, false)
}

func (c *pipeConn) Write(b []byte) (int, error) {
	written := 0
	for written < len(b) {
		n, err := c.do(b[written:], true)
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (c *pipeConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	syscall.CancelIoEx(c.h, nil)
	c.inflight.Wait()
	return syscall.CloseHandle(c.h)
}

func (c *pipeConn) LocalAddr() net.Addr  { return pipeAddr(c.path) }
func (c *pipeConn) RemoteAddr() net.Addr { return pipeAddr(c.path) }

func (c *pipeConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.rdl, c.wdl = t, t
	c.mu.Unlock()
	return nil
}

func (c *pipeConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.rdl = t
	c.mu.Unlock()
	return nil
}

func (c *pipeConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wdl = t
	c.mu.Unlock()
	return nil
}
