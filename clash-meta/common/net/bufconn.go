package net

import (
	"bufio"
	"net"

	"github.com/Dreamacro/clash/common/buf"
)

var _ ExtendedConn = (*BufferedConn)(nil)

type BufferedConn struct {
	r *bufio.Reader
	ExtendedConn
	peeked bool
}

func NewBufferedConn(c net.Conn) *BufferedConn {
	if bc, ok := c.(*BufferedConn); ok {
		return bc
	}
	return &BufferedConn{bufio.NewReader(c), NewExtendedConn(c), false}
}

// Reader returns the internal bufio.Reader.
func (c *BufferedConn) Reader() *bufio.Reader {
	return c.r
}

func (c *BufferedConn) ResetPeeked() {
	c.peeked = false
}

func (c *BufferedConn) Peeked() bool {
	return c.peeked
}

// Peek returns the next n bytes without advancing the reader.
func (c *BufferedConn) Peek(n int) ([]byte, error) {
	c.peeked = true
	return c.r.Peek(n)
}

func (c *BufferedConn) Discard(n int) (discarded int, err error) {
	return c.r.Discard(n)
}

func (c *BufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *BufferedConn) ReadByte() (byte, error) {
	return c.r.ReadByte()
}

func (c *BufferedConn) UnreadByte() error {
	return c.r.UnreadByte()
}

func (c *BufferedConn) Buffered() int {
	return c.r.Buffered()
}

func (c *BufferedConn) ReadBuffer(buffer *buf.Buffer) (err error) {
	if c.r != nil && c.r.Buffered() > 0 {
		_, err = buffer.ReadOnceFrom(c.r)
		return
	}
	return c.ExtendedConn.ReadBuffer(buffer)
}

func (c *BufferedConn) ReadCached() *buf.Buffer { // call in sing/common/bufio.Copy
	if c.r != nil && c.r.Buffered() > 0 {
		length := c.r.Buffered()
		b, _ := c.r.Peek(length)
		_, _ = c.r.Discard(length)
		c.r = nil // drop bufio.Reader to let gc can clean up its internal buf
		return buf.As(b)
	}
	return nil
}

func (c *BufferedConn) Upstream() any {
	return c.ExtendedConn
}

func (c *BufferedConn) ReaderReplaceable() bool {
	if c.r != nil && c.r.Buffered() > 0 {
		return false
	}
	return true
}

func (c *BufferedConn) WriterReplaceable() bool {
	return true
}
