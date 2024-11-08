package abstract

import (
	na "github.com/cometbft/cometbft/p2p/netaddr"
)

// Transport connects the local node to the rest of the network.
type Transport interface {
	// NetAddr returns the network address of the local node.
	NetAddr() na.NetAddr

	// Accept waits for and returns the next connection to the local node.
	Accept() (Connection, *na.NetAddr, error)

	// Dial dials the given address and returns a connection.
	Dial(addr na.NetAddr) (Connection, error)

	// Cleanup any resources associated with the given connection.
	//
	// Must be run when the peer is dropped for any reason.
	Cleanup(conn Connection) error
}
