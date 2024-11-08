package p2p

import (
	"net"
	"time"

	"github.com/cometbft/cometbft/p2p/abstract"
	na "github.com/cometbft/cometbft/p2p/netaddr"
)

type mockStream struct {
	net.Conn
}

func (s mockStream) Read(b []byte) (n int, err error) {
	return s.Conn.Read(b)
}

func (s mockStream) Write(b []byte) (n int, err error) {
	return s.Conn.Write(b)
}

func (mockStream) Close() error {
	return nil
}
func (s mockStream) SetDeadline(t time.Time) error      { return s.Conn.SetReadDeadline(t) }
func (s mockStream) SetReadDeadline(t time.Time) error  { return s.Conn.SetReadDeadline(t) }
func (s mockStream) SetWriteDeadline(t time.Time) error { return s.Conn.SetWriteDeadline(t) }

type mockConnection struct {
	net.Conn
}

func (c mockConnection) OpenStream(byte) (abstract.Stream, error) {
	return c.Conn.(mockStream), nil
}

func (c mockConnection) LocalAddr() net.Addr {
	return c.Conn.LocalAddr()
}

func (c mockConnection) RemoteAddr() net.Addr {
	return c.Conn.RemoteAddr()
}
func (c mockConnection) Close(string) error         { return c.Conn.Close() }
func (c mockConnection) FlushAndClose(string) error { return c.Conn.Close() }
func (mockConnection) ConnectionState() any         { return nil }

var _ abstract.Transport = (*mockTransport)(nil)

type mockTransport struct {
	ln   net.Listener
	addr na.NetAddr
}

func (t *mockTransport) Listen(addr na.NetAddr) error {
	ln, err := net.Listen("tcp", addr.DialString())
	if err != nil {
		return err
	}
	t.addr = addr
	t.ln = ln
	return nil
}

func (t *mockTransport) NetAddr() na.NetAddr {
	return t.addr
}

func (t *mockTransport) Accept() (abstract.Connection, *na.NetAddr, error) {
	c, err := t.ln.Accept()
	return &mockConnection{Conn: c}, nil, err
}

func (*mockTransport) Dial(addr na.NetAddr) (abstract.Connection, error) {
	c, err := addr.DialTimeout(time.Second)
	return &mockConnection{Conn: c}, err
}

func (*mockTransport) Cleanup(abstract.Connection) error {
	return nil
}
