//go:build linux

package main

import (
	"io"
	"sync"
)

// highWater is how many bytes of scanned image may sit unread before the scan
// goroutine waits for the frontend to catch up. The device is content to be
// kept waiting between image blocks, and this keeps a 600 dpi full bed - which
// is over a hundred megabytes - from being held in memory because a frontend
// reads slowly.
const highWater = 4 << 20

// stream carries image rows from the scan goroutine to sane_read.
//
// It is a pipe with one unusual property: discard. A cancelled scan must not
// stop the transfer, because a device left mid-transfer stays wedged until its
// mains power is cut, so cancelling switches the stream to throwing the rows
// away and the scan runs to its natural end in the background.
type stream struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  []byte
	done bool
	err  error
	drop bool
}

func newStream() *stream {
	s := &stream{}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Write is the scan goroutine's end. It blocks while the reader is behind.
func (s *stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buf) >= highWater && !s.drop {
		s.cond.Wait()
	}
	if s.drop {
		return len(p), nil
	}
	s.buf = append(s.buf, p...)
	s.cond.Broadcast()
	return len(p), nil
}

// finish is called by the scan goroutine when the device is done with the
// transfer, successfully or not.
func (s *stream) finish(err error) {
	s.mu.Lock()
	s.done, s.err = true, err
	s.cond.Broadcast()
	s.mu.Unlock()
}

// read blocks until there is something to hand over, and returns 0 with io.EOF
// - or the scan's error - once the transfer has ended and the buffer is empty.
func (s *stream) read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buf) == 0 && !s.done {
		s.cond.Wait()
	}
	if n := copy(p, s.buf); n > 0 {
		s.buf = append(s.buf[:0], s.buf[n:]...)
		s.cond.Broadcast()
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	return 0, io.EOF
}

// discard drops what has been buffered and lets the scan goroutine run to the
// end without anyone reading it.
func (s *stream) discard() {
	s.mu.Lock()
	s.drop, s.buf = true, nil
	s.cond.Broadcast()
	s.mu.Unlock()
}

// wait blocks until the scan goroutine has finished with the device.
func (s *stream) wait() {
	s.mu.Lock()
	for !s.done {
		s.cond.Wait()
	}
	s.mu.Unlock()
}
