// Package tunnel carries many connections over one outbound connection, so
// a computer behind NAT can serve visitors through a public relay.
//
// Wire format: every frame is a 9-byte header (type, stream id, payload
// length) followed by the payload. Each stream has a 256 KB receive window:
// a sender may only have that much unread data in flight, and the receiver
// grants more as its reader consumes it. So one slow visitor never fills
// memory or stalls the other streams sharing the connection.
package tunnel

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	frameOpen   = 1 // open a new stream
	frameData   = 2
	frameClose  = 3 // the sender has finished writing (half close)
	frameReset  = 4 // abort the stream
	frameWindow = 5 // grant the peer more send window (uint32)
	framePing   = 6
	framePong   = 7

	headerSize    = 9
	maxPayload    = 32 << 10
	initialWindow = 256 << 10

	keepalive   = 15 * time.Second
	deadTimeout = 50 * time.Second
)

var (
	ErrSessionClosed  = errors.New("tunnel: session closed")
	ErrTooManyStreams = errors.New("tunnel: too many open streams")
)

// Session multiplexes streams over one connection.
type Session struct {
	conn   net.Conn
	client bool

	wmu sync.Mutex
	w   *bufio.Writer

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32
	accept  chan *Stream

	MaxStreams int

	closed    chan struct{}
	closeOnce sync.Once
	err       error
	lastRecv  atomic.Int64
	pongs     chan struct{}

	BytesIn, BytesOut atomic.Uint64
}

// NewSession starts a session. The client opens odd stream ids, the server
// even ones, so both sides can open streams without colliding.
func NewSession(conn net.Conn, client bool) *Session {
	s := &Session{conn: conn, client: client, w: bufio.NewWriterSize(conn, 64<<10),
		streams: map[uint32]*Stream{}, accept: make(chan *Stream, 256), MaxStreams: 1024,
		closed: make(chan struct{}), pongs: make(chan struct{}, 1)}
	if client {
		s.nextID = 1
	} else {
		s.nextID = 2
	}
	s.lastRecv.Store(time.Now().UnixNano())
	go s.readLoop()
	go s.keepaliveLoop()
	return s
}

func (s *Session) Done() <-chan struct{} { return s.closed }

// Err is why the session ended.
func (s *Session) Err() error {
	<-s.closed
	return s.err
}

func (s *Session) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

// Close ends the session and every stream in it.
func (s *Session) Close() error {
	s.fail(ErrSessionClosed)
	return nil
}

func (s *Session) fail(err error) {
	s.closeOnce.Do(func() {
		s.err = err
		close(s.closed)
		_ = s.conn.Close()
		s.mu.Lock()
		for _, st := range s.streams {
			st.kill(err)
		}
		s.streams = map[uint32]*Stream{}
		s.mu.Unlock()
	})
}

func (s *Session) writeFrame(typ byte, id uint32, payload []byte) error {
	var hdr [headerSize]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], id)
	binary.BigEndian.PutUint32(hdr[5:], uint32(len(payload)))
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.closed:
		return ErrSessionClosed
	default:
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(deadTimeout))
	_, err := s.w.Write(hdr[:])
	if err == nil && len(payload) > 0 {
		_, err = s.w.Write(payload)
	}
	if err == nil {
		err = s.w.Flush()
	}
	if err != nil {
		go s.fail(err)
		return err
	}
	s.BytesOut.Add(uint64(headerSize + len(payload)))
	return nil
}

// Open starts a new stream to the peer.
func (s *Session) Open() (*Stream, error) {
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		return nil, ErrSessionClosed
	default:
	}
	if len(s.streams) >= s.MaxStreams {
		s.mu.Unlock()
		return nil, ErrTooManyStreams
	}
	id := s.nextID
	s.nextID += 2
	st := newStream(s, id)
	s.streams[id] = st
	s.mu.Unlock()
	if err := s.writeFrame(frameOpen, id, nil); err != nil {
		return nil, err
	}
	return st, nil
}

// Accept waits for a stream opened by the peer.
func (s *Session) Accept() (*Stream, error) {
	select {
	case st := <-s.accept:
		return st, nil
	case <-s.closed:
		return nil, s.err
	}
}

// Ping measures the round trip to the peer.
func (s *Session) Ping(timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	if err := s.writeFrame(framePing, 0, nil); err != nil {
		return 0, err
	}
	select {
	case <-s.pongs:
		return time.Since(start), nil
	case <-time.After(timeout):
		return 0, errors.New("tunnel: ping timed out")
	case <-s.closed:
		return 0, s.err
	}
}

func (s *Session) keepaliveLoop() {
	t := time.NewTicker(keepalive)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			if time.Since(time.Unix(0, s.lastRecv.Load())) > deadTimeout {
				s.fail(errors.New("tunnel: peer stopped responding"))
				return
			}
			_ = s.writeFrame(framePing, 0, nil)
		}
	}
}

func (s *Session) readLoop() {
	r := bufio.NewReaderSize(s.conn, 64<<10)
	var hdr [headerSize]byte
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(deadTimeout + 10*time.Second))
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			s.fail(err)
			return
		}
		typ, id, n := hdr[0], binary.BigEndian.Uint32(hdr[1:]), binary.BigEndian.Uint32(hdr[5:])
		if n > maxPayload {
			s.fail(errors.New("tunnel: frame too large"))
			return
		}
		var payload []byte
		if n > 0 {
			payload = make([]byte, n)
			if _, err := io.ReadFull(r, payload); err != nil {
				s.fail(err)
				return
			}
		}
		s.lastRecv.Store(time.Now().UnixNano())
		s.BytesIn.Add(uint64(headerSize + n))

		switch typ {
		case framePing:
			go s.writeFrame(framePong, 0, nil)
			continue
		case framePong:
			select {
			case s.pongs <- struct{}{}:
			default:
			}
			continue
		case frameOpen:
			s.mu.Lock()
			if _, dup := s.streams[id]; dup || len(s.streams) >= s.MaxStreams || (id%2 == 1) != !s.client {
				s.mu.Unlock()
				go s.writeFrame(frameReset, id, nil)
				continue
			}
			st := newStream(s, id)
			s.streams[id] = st
			s.mu.Unlock()
			select {
			case s.accept <- st:
			default: // nobody accepting fast enough
				st.kill(ErrTooManyStreams)
				s.remove(id)
				go s.writeFrame(frameReset, id, nil)
			}
			continue
		}

		s.mu.Lock()
		st := s.streams[id]
		s.mu.Unlock()
		if st == nil {
			if typ == frameData { // late data for a closed stream: still return the window
				go s.writeFrame(frameWindow, id, binary.BigEndian.AppendUint32(nil, n))
			}
			continue
		}
		switch typ {
		case frameData:
			st.receive(payload)
		case frameClose:
			st.receiveClose()
		case frameReset:
			st.kill(syscall.ECONNRESET)
			s.remove(id)
		case frameWindow:
			if len(payload) == 4 {
				st.grant(int64(binary.BigEndian.Uint32(payload)))
			}
		}
	}
}

func (s *Session) remove(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

// NumStreams is the number of open streams.
func (s *Session) NumStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

// Stream is one connection inside a session. It implements net.Conn.
type Stream struct {
	id uint32
	s  *Session

	mu          sync.Mutex
	cond        *sync.Cond
	buf         bytes.Buffer
	recvClosed  bool // peer finished writing
	localClosed bool // Close was called: stop delivering data
	writeClosed bool
	err         error // reset or session failure
	sendWindow  int64
	unacked     int // bytes read but not yet granted back

	rdl, wdl       time.Time
	rTimer, wTimer *time.Timer
}

func newStream(s *Session, id uint32) *Stream {
	st := &Stream{id: id, s: s, sendWindow: initialWindow}
	st.cond = sync.NewCond(&st.mu)
	return st
}

func (st *Stream) receive(p []byte) {
	st.mu.Lock()
	if st.localClosed || st.err != nil { // discard, but keep the peer's window moving
		st.mu.Unlock()
		go st.s.writeFrame(frameWindow, st.id, binary.BigEndian.AppendUint32(nil, uint32(len(p))))
		return
	}
	if st.buf.Len()+len(p) > initialWindow { // the peer ignored flow control
		st.mu.Unlock()
		st.kill(errors.New("tunnel: flow control violation"))
		st.s.remove(st.id)
		go st.s.writeFrame(frameReset, st.id, nil)
		return
	}
	st.buf.Write(p)
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *Stream) receiveClose() {
	st.mu.Lock()
	st.recvClosed = true
	done := st.localClosed
	st.cond.Broadcast()
	st.mu.Unlock()
	if done {
		st.s.remove(st.id)
	}
}

func (st *Stream) grant(n int64) {
	st.mu.Lock()
	st.sendWindow += n
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *Stream) kill(err error) {
	st.mu.Lock()
	if st.err == nil {
		st.err = err
	}
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	for st.buf.Len() == 0 && !st.recvClosed && st.err == nil && !st.localClosed {
		if !st.rdl.IsZero() && !time.Now().Before(st.rdl) {
			st.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		st.cond.Wait()
	}
	if st.buf.Len() > 0 {
		n, _ := st.buf.Read(p)
		st.unacked += n
		var grant int
		if st.unacked >= initialWindow/4 {
			grant, st.unacked = st.unacked, 0
		}
		st.mu.Unlock()
		if grant > 0 {
			_ = st.s.writeFrame(frameWindow, st.id, binary.BigEndian.AppendUint32(nil, uint32(grant)))
		}
		return n, nil
	}
	defer st.mu.Unlock()
	switch {
	case st.localClosed:
		return 0, net.ErrClosed
	case st.err != nil && !st.recvClosed:
		return 0, st.err
	}
	return 0, io.EOF
}

func (st *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		st.mu.Lock()
		for st.sendWindow <= 0 && st.err == nil && !st.writeClosed {
			if !st.wdl.IsZero() && !time.Now().Before(st.wdl) {
				st.mu.Unlock()
				return written, os.ErrDeadlineExceeded
			}
			st.cond.Wait()
		}
		if st.err != nil {
			err := st.err
			st.mu.Unlock()
			return written, err
		}
		if st.writeClosed {
			st.mu.Unlock()
			return written, net.ErrClosed
		}
		n := min(int64(len(p)), st.sendWindow, maxPayload)
		st.sendWindow -= n
		st.mu.Unlock()
		if err := st.s.writeFrame(frameData, st.id, p[:n]); err != nil {
			return written, err
		}
		written += int(n)
		p = p[n:]
	}
	return written, nil
}

// CloseWrite tells the peer no more data is coming (it reads io.EOF).
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.writeClosed || st.err != nil {
		st.mu.Unlock()
		return nil
	}
	st.writeClosed = true
	st.cond.Broadcast()
	st.mu.Unlock()
	return st.s.writeFrame(frameClose, st.id, nil)
}

// Close finishes the stream from this side. Data still arriving is
// discarded (with its window returned), so the peer never blocks on it.
func (st *Stream) Close() error {
	err := st.CloseWrite()
	st.mu.Lock()
	st.localClosed = true
	done := st.recvClosed || st.err != nil
	pending := st.buf.Len() + st.unacked
	st.buf.Reset()
	st.unacked = 0
	st.cond.Broadcast()
	st.mu.Unlock()
	if pending > 0 {
		_ = st.s.writeFrame(frameWindow, st.id, binary.BigEndian.AppendUint32(nil, uint32(pending)))
	}
	if done {
		st.s.remove(st.id)
	}
	return err
}

func (st *Stream) LocalAddr() net.Addr  { return st.s.conn.LocalAddr() }
func (st *Stream) RemoteAddr() net.Addr { return st.s.conn.RemoteAddr() }

func (st *Stream) SetDeadline(t time.Time) error {
	_ = st.SetReadDeadline(t)
	return st.SetWriteDeadline(t)
}

func (st *Stream) SetReadDeadline(t time.Time) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rdl = t
	st.rTimer = rearm(st.rTimer, t, st.cond)
	st.cond.Broadcast()
	return nil
}

func (st *Stream) SetWriteDeadline(t time.Time) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.wdl = t
	st.wTimer = rearm(st.wTimer, t, st.cond)
	st.cond.Broadcast()
	return nil
}

func rearm(tm *time.Timer, t time.Time, c *sync.Cond) *time.Timer {
	if tm != nil {
		tm.Stop()
	}
	if t.IsZero() {
		return nil
	}
	return time.AfterFunc(time.Until(t), func() {
		c.L.Lock()
		c.Broadcast()
		c.L.Unlock()
	})
}
