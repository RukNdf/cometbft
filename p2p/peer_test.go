package p2p

import (
	"errors"
	"fmt"
	golog "log"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	p2p "github.com/cometbft/cometbft/api/cometbft/p2p/v1"
	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/crypto/ed25519"
	"github.com/cometbft/cometbft/libs/bytes"
	"github.com/cometbft/cometbft/libs/log"
	na "github.com/cometbft/cometbft/p2p/netaddr"
	ni "github.com/cometbft/cometbft/p2p/nodeinfo"
	"github.com/cometbft/cometbft/p2p/nodekey"
	tcpconn "github.com/cometbft/cometbft/p2p/transport/tcp/conn"
)

func TestPeerBasic(t *testing.T) {
	rp := &remoteTCPPeer{PrivKey: ed25519.GenPrivKey(), Config: cfg}
	rp.Start()
	defer rp.Stop()

	p, err := createOutboundPeerAndPerformHandshake(t, rp.Addr(), cfg, tcpconn.DefaultMConnConfig())
	require.NoError(t, err)

	err = p.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Error(err)
		}
	})

	assert.True(t, p.IsRunning())
	assert.True(t, p.IsOutbound())

	assert.False(t, p.IsPersistent())
	p.persistent = true
	assert.True(t, p.IsPersistent())

	assert.Equal(t, rp.Addr().DialString(), p.RemoteAddr().String())
	assert.Equal(t, rp.ID(), p.ID())
}

func TestPeerSend(t *testing.T) {
	config := cfg

	rp := &remoteTCPPeer{PrivKey: ed25519.GenPrivKey(), Config: config}
	rp.Start()
	defer rp.Stop()

	p, err := createOutboundPeerAndPerformHandshake(t, rp.Addr(), config, tcpconn.DefaultMConnConfig())
	require.NoError(t, err)

	err = p.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Error(err)
		}
	})

	// TODO: uncomment or remove
	// assert.True(p.CanSend(testCh))
	assert.True(t, p.Send(Envelope{ChannelID: testCh, Message: &p2p.Message{}}))
}

func createOutboundPeerAndPerformHandshake(
	t *testing.T,
	addr *na.NetAddr,
	config *config.P2PConfig,
	mConfig tcpconn.MConnConfig,
) (*peer, error) {
	t.Helper()

	pc, err := testOutboundPeerConn(addr, config, false)
	require.NoError(t, err)

	stream, err := pc.OpenStream(handshakeStreamID)
	require.NoError(t, err)
	defer stream.Close()

	// create dummy node info and perform handshake
	var (
		timeout     = 1 * time.Second
		ourNodeID   = nodekey.PubKeyToID(ed25519.GenPrivKey().PubKey())
		ourNodeInfo = testNodeInfo(ourNodeID, "host_peer")
	)
	peerNodeInfo, err := handshake(ourNodeInfo, stream, timeout)
	require.NoError(t, err)

	// create peer
	var (
		streamDescs = []StreamDescriptor{
			&tcpconn.ChannelDescriptor{
				ID:           testCh,
				Priority:     1,
				MessageTypeI: &p2p.Message{},
			},
		}
		streamInfoByStreamID = map[byte]streamInfo{
			testCh: streamInfo{
				reactor: NewTestReactor(streamDescs, true),
				msgType: &p2p.Message{},
			},
		}
	)
	p, err := newPeer(pc, mConfig, peerNodeInfo, streamInfoByStreamID, func(_ Peer, _ any) {})
	require.NoError(t, err)
	p.SetLogger(log.TestingLogger().With("peer", addr))
	return p, nil
}

func testDial(addr *na.NetAddr, cfg *config.P2PConfig) (Connection, error) {
	if cfg.TestDialFail {
		return nil, errors.New("dial err (peerConfig.DialFail == true)")
	}
	conn, err := addr.DialTimeout(cfg.DialTimeout)
	if err != nil {
		return nil, err
	}
	return mockConnection{conn}, nil
}

// testOutboundPeerConn dials a remote peer and returns a peerConn.
// It ensures the dialed ID matches the connection ID.
func testOutboundPeerConn(addr *na.NetAddr, config *config.P2PConfig, persistent bool) (peerConn, error) {
	var pc peerConn

	conn, err := testDial(addr, config)
	if err != nil {
		return pc, fmt.Errorf("creating peer: %w", err)
	}

	pc, err = testPeerConn(conn, config, true, persistent, addr)
	if err != nil {
		_ = conn.Close(err.Error())
		return pc, err
	}

	if addr.ID != pc.ID() { // ensure dialed ID matches connection ID
		_ = conn.Close(err.Error())
		return pc, ErrSwitchAuthenticationFailure{addr, pc.ID()}
	}

	return pc, nil
}

type remoteTCPPeer struct {
	PrivKey    crypto.PrivKey
	Config     *config.P2PConfig
	addr       *na.NetAddr
	channels   bytes.HexBytes
	listenAddr string
	listener   net.Listener
}

func (rp *remoteTCPPeer) Addr() *na.NetAddr {
	return rp.addr
}

func (rp *remoteTCPPeer) ID() nodekey.ID {
	return nodekey.PubKeyToID(rp.PrivKey.PubKey())
}

func (rp *remoteTCPPeer) Start() {
	if rp.listenAddr == "" {
		rp.listenAddr = "127.0.0.1:0"
	}

	l, e := net.Listen("tcp", rp.listenAddr) // any available address
	if e != nil {
		golog.Fatalf("net.Listen tcp :0: %+v", e)
	}
	rp.listener = l
	rp.addr = na.New(nodekey.PubKeyToID(rp.PrivKey.PubKey()), l.Addr())
	if rp.channels == nil {
		rp.channels = []byte{testCh}
	}
	go rp.accept()
}

func (rp *remoteTCPPeer) Stop() {
	rp.listener.Close()
}

func (rp *remoteTCPPeer) Dial(addr *na.NetAddr) (Connection, error) {
	pc, err := testOutboundPeerConn(addr, rp.Config, false)
	if err != nil {
		return nil, err
	}

	stream, err := pc.OpenStream(handshakeStreamID)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	_, err = handshake(rp.nodeInfo(), stream, time.Second)
	if err != nil {
		return nil, err
	}
	return pc, err
}

func (rp *remoteTCPPeer) accept() {
	conns := []peerConn{}

	for {
		netConn, err := rp.listener.Accept()
		if err != nil {
			golog.Printf("Failed to accept conn: %+v", err)
			for _, conn := range conns {
				_ = conn.Close(err.Error())
			}
			return
		}

		conn := mockConnection{netConn}

		pc, err := testInboundPeerConn(conn, rp.Config)
		if err != nil {
			_ = conn.Close(err.Error())
			golog.Fatalf("Failed to create a peer: %+v", err)
		}

		stream, err := conn.OpenStream(handshakeStreamID)
		if err != nil {
			_ = pc.Close(err.Error())
			golog.Fatalf("Failed to open the handshake stream: %+v", err)
		}
		defer stream.Close()

		_, err = handshake(rp.nodeInfo(), stream, time.Second)
		if err != nil {
			_ = pc.Close(err.Error())
			golog.Printf("Failed to perform handshake: %+v", err)
		}

		conns = append(conns, pc)
	}
}

func (rp *remoteTCPPeer) nodeInfo() ni.NodeInfo {
	la := rp.listener.Addr().String()
	nodeInfo := testNodeInfo(rp.ID(), "remote_peer_"+la)
	nodeInfo.ListenAddr = la
	nodeInfo.Channels = rp.channels
	return nodeInfo
}
