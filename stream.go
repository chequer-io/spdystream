package spdystream

import (
	"code.google.com/p/go.net/spdy"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

type Stream struct {
	streamId  spdy.StreamId
	parent    *Stream
	conn      *Connection
	startChan chan error

	dataLock sync.RWMutex
	dataChan chan []byte
	unread   []byte

	priority   uint8
	headers    http.Header
	headerChan chan http.Header
	finishLock sync.Mutex
	finished   bool
	replyCond  *sync.Cond
	replied    bool
	closeChan  chan bool
}

// WriteData writes data to stream, sending a dataframe per call
func (s *Stream) WriteData(data []byte, fin bool) error {
	s.waitWriteReply()
	var flags spdy.DataFlags

	if fin {
		flags = spdy.DataFlagFin
		s.finishLock.Lock()
		if s.finished {
			s.finishLock.Unlock()
			return ErrWriteClosedStream
		}
		s.finished = true
		s.finishLock.Unlock()
	}

	dataFrame := &spdy.DataFrame{
		StreamId: s.streamId,
		Flags:    flags,
		Data:     data,
	}

	s.conn.writeLock.Lock()
	defer s.conn.writeLock.Unlock()
	return s.conn.framer.WriteFrame(dataFrame)
}

// Write writes bytes to a stream, calling write data for each call.
func (s *Stream) Write(data []byte) (n int, err error) {
	err = s.WriteData(data, false)
	if err == nil {
		n = len(data)
	}
	return
}

// Read reads bytes from a stream, a single read will never get more
// than what is sent on a single data frame, but a multiple calls to
// read may get data from the same data frame.
func (s *Stream) Read(p []byte) (n int, err error) {
	if s.unread == nil {
		select {
		case <-s.closeChan:
			return 0, io.EOF
		case read, ok := <-s.dataChan:
			if !ok {
				return 0, io.EOF
			}
			s.unread = read
		}
	}
	n = copy(p, s.unread)
	if n < len(s.unread) {
		s.unread = s.unread[n:]
	} else {
		s.unread = nil
	}
	return
}

func (s *Stream) waitWriteReply() {
	if s.replyCond != nil {
		s.replyCond.L.Lock()
		for !s.replied {
			s.replyCond.Wait()
		}
		s.replyCond.L.Unlock()
	}
}

// Wait waits for the stream to receive a reply.
func (s *Stream) Wait() error {
	return s.WaitTimeout(time.Duration(0))
}

// WaitTimeout waits for the stream to receive a reply or for timeout.
// When the timeout is reached, ErrTimeout will be returned.
func (s *Stream) WaitTimeout(timeout time.Duration) error {
	var timeoutChan <-chan time.Time
	if timeout > time.Duration(0) {
		timeoutChan = time.After(timeout)
	}

	select {
	case err := <-s.startChan:
		if err != nil {
			return err
		}
		break
	case <-timeoutChan:
		return ErrTimeout
	}
	return nil
}

// Close closes the stream by sending an empty data frame with the
// finish flag set, indicating this side is finished with the stream.
func (s *Stream) Close() error {
	select {
	case <-s.closeChan:
		// Stream is now fully closed
		s.conn.removeStream(s)
	default:
		break
	}
	return s.WriteData([]byte{}, true)
}

// Reset sends a reset frame, putting the stream into the fully closed state.
func (s *Stream) Reset() error {
	s.conn.removeStream(s)

	s.finishLock.Lock()
	if s.finished {
		s.finishLock.Unlock()
		return nil
	}
	s.finished = true
	s.finishLock.Unlock()

	s.dataLock.Lock()
	select {
	case <-s.closeChan:
		break
	default:
		close(s.dataChan)
		close(s.closeChan)
	}
	s.dataLock.Unlock()

	resetFrame := &spdy.RstStreamFrame{
		StreamId: s.streamId,
		Status:   spdy.Cancel,
	}
	s.conn.writeLock.Lock()
	defer s.conn.writeLock.Unlock()
	return s.conn.framer.WriteFrame(resetFrame)
}

// CreateSubStream creates a stream using the current as the parent
func (s *Stream) CreateSubStream(headers http.Header, fin bool) (*Stream, error) {
	return s.conn.CreateStream(headers, s, fin)
}

// SetPriority sets the stream priority, does not affect the
// remote priority of this stream after Open has been called.
// Valid values are 0 through 7, 0 being the highest priority
// and 7 the lowest.
func (s *Stream) SetPriority(priority uint8) {
	s.priority = priority
}

// SendHeader sends a header frame across the stream
func (s *Stream) SendHeader(headers http.Header, fin bool) error {
	return s.conn.sendHeaders(headers, s, fin)
}

// SendReply sends a reply on a stream, only valid to be called once
// when handling a new stream
func (s *Stream) SendReply(headers http.Header, fin bool) error {
	if s.replyCond == nil {
		return errors.New("cannot reply on initiated stream")
	}
	s.replyCond.L.Lock()
	defer s.replyCond.L.Unlock()
	if s.replied {
		return nil
	}

	err := s.conn.sendReply(headers, s, fin)
	if err != nil {
		return err
	}

	s.replied = true
	s.replyCond.Broadcast()
	return nil
}

// Refuse sends a reset frame with the status refuse, only
// valid to be called once when handling a new stream.  This
// may be used to indicate that a stream is not allowed
// when http status codes are not being used.
func (s *Stream) Refuse() error {
	if s.replied {
		return nil
	}
	s.replied = true
	return s.conn.sendReset(spdy.RefusedStream, s)
}

// Cancel sends a reset frame with the status canceled. This
// can be used at any time by the creator of the Stream to
// indicate the stream is no longer needed.
func (s *Stream) Cancel() error {
	return s.conn.sendReset(spdy.Cancel, s)
}

// ReceiveHeader receives a header sent on the other side
// of the stream.  This function will block until a header
// is received or stream is closed.
func (s *Stream) ReceiveHeader() (http.Header, error) {
	select {
	case <-s.closeChan:
		break
	case header, ok := <-s.headerChan:
		if !ok {
			return nil, fmt.Errorf("header chan closed")
		}
		return header, nil
	}
	return nil, fmt.Errorf("stream closed")
}

// Parent returns the parent stream
func (s *Stream) Parent() *Stream {
	return s.parent
}

// Headers returns the headers used to create the stream
func (s *Stream) Headers() http.Header {
	return s.headers
}

// String returns the string version of stream using the
// streamId to uniquely identify the stream
func (s *Stream) String() string {
	return fmt.Sprintf("stream:%d", s.streamId)
}

// IsFinished returns whether the stream has finished
// sending data
func (s *Stream) IsFinished() bool {
	return s.finished
}

// Implement net.Conn interface

func (s *Stream) LocalAddr() net.Addr {
	return s.conn.conn.LocalAddr()
}

func (s *Stream) RemoteAddr() net.Addr {
	return s.conn.conn.RemoteAddr()
}

// TODO set per stream values instead of connection-wide

func (s *Stream) SetDeadline(t time.Time) error {
	return s.conn.conn.SetDeadline(t)
}

func (s *Stream) SetReadDeadline(t time.Time) error {
	return s.conn.conn.SetReadDeadline(t)
}

func (s *Stream) SetWriteDeadline(t time.Time) error {
	return s.conn.conn.SetWriteDeadline(t)
}
