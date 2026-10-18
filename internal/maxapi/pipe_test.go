package maxapi

import (
	"net"
	"time"
)

type pipeEnd struct{ net.Conn }

// newPipe returns the far end (for assertions) and the near end (given to the client).
func newPipe() (*pipeEnd, net.Conn) {
	a, b := net.Pipe()
	return &pipeEnd{a}, b
}

// waitClosed reports whether the peer closed its end within d (a read then fails).
func (p *pipeEnd) waitClosed(d time.Duration) bool {
	_ = p.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 1)
	_, err := p.Read(buf)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return false
	}
	return err != nil
}
