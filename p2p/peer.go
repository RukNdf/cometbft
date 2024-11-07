package p2p

import (
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/cosmos/gogoproto/proto"

	"github.com/cometbft/cometbft/internal/cmap"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/libs/service"
	na "github.com/cometbft/cometbft/p2p/netaddr"
	ni "github.com/cometbft/cometbft/p2p/nodeinfo"
	"github.com/cometbft/cometbft/p2p/nodekey"
	tcpconn "github.com/cometbft/cometbft/p2p/transport/tcp/conn"
	"github.com/cometbft/cometbft/types"
)

//go:generate ../scripts/mockery_generate.sh Peer

// Same as the default Prometheus scrape interval in order to not lose
// granularity.
const metricsTickerDuration = 1 * time.Second

// peerConfig is used to bundle data we need to fully setup a Peer with an
// MConn, provided by the caller of Accept and Dial (currently the Switch). This
// a temporary measure until reactor setup is less dynamic and we introduce the
// concept of PeerBehaviour to communicate about significant Peer lifecycle
// events.
// TODO(xla): Refactor out with more static Reactor setup and PeerBehaviour.
type peerConfig struct {
	streamDescs []StreamDescriptor
	onPeerError func(Peer, any)
	outbound    bool
	// isPersistent allows you to set a function, which, given socket address
	// (for outbound peers) OR self-reported address (for inbound peers), tells
	// if the peer is persistent or not.
	isPersistent       func(*na.NetAddr) bool
	reactorsByStreamID map[byte]Reactor
	msgTypeByStreamID  map[byte]proto.Message
	metrics            *Metrics
}

// Peer is an interface representing a peer connected on a reactor.
type Peer interface {
	service.Service
	FlushStop()

	ID() nodekey.ID       // peer's cryptographic ID
	RemoteIP() net.IP     // remote IP of the connection
	RemoteAddr() net.Addr // remote address of the connection

	IsOutbound() bool   // did we dial the peer
	IsPersistent() bool // do we redial this peer when we disconnect

	// Conn returns the underlying connection.
	Conn() Connection

	NodeInfo() ni.NodeInfo // peer's info
	Status() tcpconn.ConnectionStatus
	SocketAddr() *na.NetAddr // actual address of the socket

	HasChannel(chID byte) bool // Does the peer implement this channel?
	Send(e Envelope) bool      // Send a message to the peer, blocking version
	TrySend(e Envelope) bool   // Send a message to the peer, non-blocking version

	Set(key string, value any)
	Get(key string) any

	SetRemovalFailed()
	GetRemovalFailed() bool
}

// ----------------------------------------------------------

// peerConn contains the raw connection and its config.
type peerConn struct {
	outbound   bool
	persistent bool
	Connection // Source connection

	socketAddr *na.NetAddr

	// cached RemoteIP()
	ip net.IP
}

func newPeerConn(
	outbound, persistent bool,
	conn Connection,
	socketAddr *na.NetAddr,
) peerConn {
	return peerConn{
		outbound:   outbound,
		persistent: persistent,
		Connection:       conn,
		socketAddr: socketAddr,
	}
}

// ID returns the peer's ID.
//
// Only used in tests.
func (pc peerConn) ID() nodekey.ID {
	return pc.socketAddr.ID
}

// Return the IP from the connection RemoteAddr.
func (pc peerConn) RemoteIP() net.IP {
	if pc.ip != nil {
		return pc.ip
	}

	host, _, err := net.SplitHostPort(pc.RemoteAddr().String())
	if err != nil {
		panic(err)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		panic(err)
	}

	pc.ip = ips[0]

	return pc.ip
}

// peer implements Peer.
//
// Before using a peer, you will need to perform a handshake on connection.
type peer struct {
	service.BaseService

	peerConn

	// peer's node info and the channel it knows about
	// channels = nodeInfo.Channels
	// cached to avoid copying nodeInfo in HasChannel
	nodeInfo ni.NodeInfo
	channels []byte

	// User data
	Data *cmap.CMap

	metrics        *Metrics
	pendingMetrics *peerPendingMetricsCache

	// When removal of a peer fails, we set this flag
	removalAttemptFailed bool
}

type PeerOption func(*peer)

func newPeer(
	pc peerConn,
	mConfig tcpconn.MConnConfig,
	nodeInfo ni.NodeInfo,
	reactorsByStreamID map[byte]Reactor,
	msgTypeByStreamID map[byte]proto.Message,
	onPeerError func(Peer, any),
	options ...PeerOption,
) *peer {
	p := &peer{
		peerConn:       pc,
		nodeInfo:       nodeInfo,
		channels:       nodeInfo.(ni.Default).Channels,
		Data:           cmap.NewCMap(),
		metrics:        NopMetrics(),
		pendingMetrics: newPeerPendingMetricsCache(),
	}

	go p.readLoop(reactorsByStreamID, msgTypeByStreamID)

	p.BaseService = *service.NewBaseService(nil, "Peer", p)
	for _, option := range options {
		option(p)
	}

	return p
}

func (p *peer) readLoop(reactorsByStreamID map[byte]Reactor, msgTypeByStreamID map[byte]proto.Message) {
	for {
		select {
		case <-p.Quit():
			return
		default:
			// TODO: establish priority for reading from streams (consensus -> evidence -> mempool).
			for streamID, reactor := range reactorsByStreamID {
				buf := make([]byte, 1024) // TODO max msg size for this stream

				n, err := p.peerConn.Read(streamID, buf)
				if err != nil {
					p.Logger.Debug("Error reading from stream", "stream", streamID,  "err", err)
					p.Stop()
					return
				}
				if n == 0 {
					continue
				}

				mt := msgTypeByStreamID[streamID]
				msg := proto.Clone(mt)
				err = proto.Unmarshal(buf[:n], msg)
				if err != nil {
					panic(fmt.Sprintf("unmarshaling message: %v into type: %s", err, reflect.TypeOf(mt)))
				}

				if w, ok := msg.(types.Unwrapper); ok {
					msg, err = w.Unwrap()
					if err != nil {
						panic(fmt.Sprintf("unwrapping message: %v", err))
					}
				}

				p.pendingMetrics.AddPendingRecvBytes(getMsgType(msg), n)
				reactor.Receive(Envelope{
					ChannelID: streamID,
					Src:       p,
					Message:   msg,
				})
			}
		}
	}
}

// String representation.
func (p *peer) String() string {
	if p.outbound {
		return fmt.Sprintf("Peer{%v %v out}", p.conn, p.ID())
	}

	return fmt.Sprintf("Peer{%v %v in}", p.conn, p.ID())
}

// ---------------------------------------------------
// Implements service.Service

// SetLogger implements BaseService.
func (p *peer) SetLogger(l log.Logger) {
	p.Logger = l
	// p.mconn.SetLogger(l)
}

// OnStart implements BaseService.
func (p *peer) OnStart() error {
	if err := p.BaseService.OnStart(); err != nil {
		return err
	}

	// if err := p.mconn.Start(); err != nil {
	// 	return err
	// }

	go p.metricsReporter()
	return nil
}

// FlushStop mimics OnStop but additionally ensures that all successful
// .Send() calls will get flushed before closing the connection.
//
// NOTE: it is not safe to call this method more than once.
func (p *peer) FlushStop() {
	p.conn.FlushAndClose("stopping peer") // stop everything and close the conn
}

// OnStop implements BaseService.
func (p *peer) OnStop() {
	// if err := p.mconn.Stop(); err != nil { // stop everything and close the conn
	// 	p.Logger.Debug("Error while stopping peer", "err", err)
	// }
	p.conn.Close("stopping peer")
}

// ---------------------------------------------------
// Implements Peer

// ID returns the peer's ID - the hex encoded hash of its pubkey.
func (p *peer) ID() nodekey.ID {
	return p.nodeInfo.ID()
}

// IsOutbound returns true if the connection is outbound, false otherwise.
func (p *peer) IsOutbound() bool {
	return p.peerConn.outbound
}

// IsPersistent returns true if the peer is persistent, false otherwise.
func (p *peer) IsPersistent() bool {
	return p.peerConn.persistent
}

// NodeInfo returns a copy of the peer's NodeInfo.
func (p *peer) NodeInfo() ni.NodeInfo {
	return p.nodeInfo
}

// SocketAddr returns the address of the socket.
// For outbound peers, it's the address dialed (after DNS resolution).
// For inbound peers, it's the address returned by the underlying connection
// (not what's reported in the peer's NodeInfo).
func (p *peer) SocketAddr() *na.NetAddr {
	return p.peerConn.socketAddr
}

// Status returns the peer's ConnectionStatus.
func (p *peer) Status() any {
	return p.conn.ConnectionState()
}

// Send msg bytes to the channel identified by chID byte. Returns false if the
// send queue is full after timeout, specified by MConnection.
//
// thread safe.
func (p *peer) Send(e Envelope) bool {
	p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return p.send(e.ChannelID, e.Message, p.conn.Write)
}

// TrySend msg bytes to the channel identified by chID byte. Immediately returns
// false if the send queue is full.
//
// thread safe.
func (p *peer) TrySend(e Envelope) bool {
	p.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	return p.send(e.ChannelID, e.Message, p.conn.Write)
}

func (p *peer) send(streamID byte, msg proto.Message, sendFunc func(byte, []byte) (int, error)) bool {
	if !p.IsRunning() {
		return false
	} else if !p.HasChannel(streamID) {
		return false
	}

	msgType := getMsgType(msg)
	if w, ok := msg.(types.Wrapper); ok {
		msg = w.Wrap()
	}

	msgBytes, err := proto.Marshal(msg)
	if err != nil {
		p.Logger.Error("Can't marshal msg", "err", err, "msg", msg)
		return false
	}

	n, err := sendFunc(streamID, msgBytes)
	if err != nil {
		p.Logger.Error("Failed to send msg to stream", "err", err, "streamID", streamID, "msg", msg)
		return false
	} else if n != len(msgBytes) {
		p.Logger.Error("Message not sent completely", "streamID", streamID, "msg", msg, "n", n, "len", len(msgBytes))
		return false
	}

	p.pendingMetrics.AddPendingSendBytes(msgType, n)
	return true
}

// Get the data for a given key.
//
// thread safe.
func (p *peer) Get(key string) any {
	return p.Data.Get(key)
}

// Set sets the data for the given key.
//
// thread safe.
func (p *peer) Set(key string, data any) {
	p.Data.Set(key, data)
}

// HasChannel returns whether the peer reported implementing this channel.
func (p *peer) HasChannel(chID byte) bool {
	for _, ch := range p.channels {
		if ch == chID {
			return true
		}
	}
	return false
}

// Conn returns the underlying peer source connection.
func (p *peer) Conn() Connection {
	return p.peerConn.conn
}

func (p *peer) SetRemovalFailed() {
	p.removalAttemptFailed = true
}

func (p *peer) GetRemovalFailed() bool {
	return p.removalAttemptFailed
}

// ---------------------------------------------------
// methods only used for testing
// TODO: can we remove these?

// RemoteAddr returns peer's remote network address.
func (p *peer) RemoteAddr() net.Addr {
	return p.peerConn.conn.RemoteAddr()
}

// CanSend returns true if the send queue is not full, false otherwise.
// func (p *peer) CanSend(chID byte) bool {
// 	if !p.IsRunning() {
// 		return false
// 	}
// 	return p.conn.CanSend(chID)
// }

// ---------------------------------------------------

func PeerMetrics(metrics *Metrics) PeerOption {
	return func(p *peer) {
		p.metrics = metrics
	}
}

func (p *peer) metricsReporter() {
	metricsTicker := time.NewTicker(metricsTickerDuration)
	defer metricsTicker.Stop()

	for {
		select {
		case <-metricsTicker.C:
			status := p.mconn.Status()
			var sendQueueSize float64
			for _, chStatus := range status.Channels {
				sendQueueSize += float64(chStatus.SendQueueSize)
			}

			p.metrics.RecvRateLimiterDelay.With("peer_id", string(p.ID())).
				Add(status.RecvMonitor.SleepTime.Seconds())
			p.metrics.SendRateLimiterDelay.With("peer_id", string(p.ID())).
				Add(status.SendMonitor.SleepTime.Seconds())

			p.metrics.PeerPendingSendBytes.With("peer_id", string(p.ID())).Set(sendQueueSize)
			// Report per peer, per message total bytes, since the last interval
			func() {
				p.pendingMetrics.mtx.Lock()
				defer p.pendingMetrics.mtx.Unlock()
				for _, entry := range p.pendingMetrics.perMessageCache {
					if entry.pendingSendBytes > 0 {
						p.metrics.MessageSendBytesTotal.
							With("message_type", entry.label).
							Add(float64(entry.pendingSendBytes))
						entry.pendingSendBytes = 0
					}
					if entry.pendingRecvBytes > 0 {
						p.metrics.MessageReceiveBytesTotal.
							With("message_type", entry.label).
							Add(float64(entry.pendingRecvBytes))
						entry.pendingRecvBytes = 0
					}
				}
			}()

		case <-p.Quit():
			return
		}
	}
}

// ------------------------------------------------------------------
// helper funcs

func createMConnection(
	conn net.Conn,
	p *peer,
	reactorsByStreamID map[byte]Reactor,
	msgTypeByStreamID map[byte]proto.Message,
	streamDescs []StreamDescriptor,
	onPeerError func(Peer, any),
	config tcpconn.MConnConfig,
) *tcpconn.MConnection {
	onReceive := func(chID byte, msgBytes []byte) {
	}

	onError := func(r any) {
		onPeerError(p, r)
	}

	// filter out non-tcpconn.ChannelDescriptor streams
	tcpDescs := make([]*tcpconn.ChannelDescriptor, 0, len(streamDescs))
	for _, stream := range streamDescs {
		var ok bool
		d, ok := stream.(*tcpconn.ChannelDescriptor)
		if !ok {
			continue
		}
		tcpDescs = append(tcpDescs, d)
	}

	return tcpconn.NewMConnectionWithConfig(
		conn,
		tcpDescs,
		onReceive,
		onError,
		config,
	)
}

func wrapPeer(c Connection, ni ni.NodeInfo, cfg peerConfig, socketAddr *na.NetAddr, mConfig tcpconn.MConnConfig) Peer {
	persistent := false
	if cfg.isPersistent != nil {
		if cfg.outbound {
			persistent = cfg.isPersistent(socketAddr)
		} else {
			selfReportedAddr, err := ni.NetAddr()
			if err == nil {
				persistent = cfg.isPersistent(selfReportedAddr)
			}
		}
	}

	peerConn := newPeerConn(
		cfg.outbound,
		persistent,
		c,
		socketAddr,
	)

	p := newPeer(
		peerConn,
		mConfig,
		ni,
		cfg.reactorsByStreamID,
		cfg.msgTypeByStreamID,
		cfg.onPeerError,
		PeerMetrics(cfg.metrics),
	)

	return p
}
