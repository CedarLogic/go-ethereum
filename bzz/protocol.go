package bzz

/*
BZZ implements the bzz wire protocol of swarm
routing decoded storage and retrieval requests
registering peers with the KAD DHT via hive
*/

import (
	"bytes"
	"fmt"
	"net"
	"path"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common/kademlia"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/errs"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/syndtr/goleveldb/leveldb/iterator"
)

const (
	Version            = 0
	ProtocolLength     = uint64(8)
	ProtocolMaxMsgSize = 10 * 1024 * 1024
	NetworkId          = 0
	strategy           = 0
)

// bzz protocol message codes
const (
	statusMsg          = iota // 0x01
	storeRequestMsg           // 0x02
	retrieveRequestMsg        // 0x03
	peersMsg                  // 0x04
)

const (
	ErrMsgTooLarge = iota
	ErrDecode
	ErrInvalidMsgCode
	ErrVersionMismatch
	ErrNetworkIdMismatch
	ErrNoStatusMsg
	ErrExtraStatusMsg
)

var errorToString = map[int]string{
	ErrMsgTooLarge:       "Message too long",
	ErrDecode:            "Invalid message",
	ErrInvalidMsgCode:    "Invalid message code",
	ErrVersionMismatch:   "Protocol version mismatch",
	ErrNetworkIdMismatch: "NetworkId mismatch",
	ErrNoStatusMsg:       "No status message",
	ErrExtraStatusMsg:    "Extra status message",
}

// bzzProtocol represents the swarm wire protocol
// instance is running on each peer
type bzzProtocol struct {
	node      *discover.Node
	netStore  *netStore
	peer      *p2p.Peer
	key       Key
	rw        p2p.MsgReadWriter
	errors    *errs.Errors
	requestDb *LDBDatabase
	quitC     chan bool
}

/*
 message structs used for rlp decoding
Handshake

[0x01, Version: B_32, strategy: B_32, capacity: B_64, peers: B_8]

Storing

[+0x02, key: B_256, metadata: [], data: B_4k]: the data chunk to be stored, preceded by its key.

Retrieving

[0x03, key: B_256, timeout: B_64, metadata: []]: key of the data chunk to be retrieved, timeout in milliseconds. Note that zero timeout retrievals serve also as messages to retrieve peers.

Peers

[0x04, key: B_256, timeout: B_64, peers: [[peer], [peer], .... ]] the encoding of a peer is identical to that in the devp2p base protocol peers messages: [IP, Port, NodeID] note that a node's DPA address is not the NodeID but the hash of the NodeID. Timeout serves to indicate whether the responder is forwarding the query within the timeout or not.

*/

type statusMsgData struct {
	Version   uint64
	ID        string
	NodeID    []byte
	Addr      *peerAddr
	NetworkId uint64
	Caps      []p2p.Cap
	// Strategy  uint64
}

func (self *statusMsgData) String() string {
	return fmt.Sprintf("Status: Version: %v, ID: %v, NodeID: %v, Addr: %v, NetworkId: %v, Caps: %v", self.Version, self.ID, self.NodeID, self.Addr, self.NetworkId, self.Caps)
}

/*
 Given the chunker I see absolutely no reason why not allow storage and delivery of larger data . See my discussion on flexible chunking.
 store requests are forwarded to the peers in their cademlia proximity bin if they are distant
 if they are within our storage radius or have any incentive to store it then attach your nodeID to the metadata
 if the storage request is sufficiently close (within our proximity range (the last row of the routing table), then sending it to all peers will not guarantee convergence, so there needs to be an absolute expiry of the request too. Maybe the protocol should specify a forward probability exponentially declining with age.
*/
type storeRequestMsgData struct {
	Key   Key    // hash of datasize | data
	SData []byte // is this needed?
	// optional
	Id             uint64     //
	requestTimeout *time.Time // expiry for forwarding
	storageTimeout *time.Time // expiry of content
	Metadata       metaData   //
	//
	peer *peer
}

func (self storeRequestMsgData) String() string {
	var from string
	if self.peer == nil {
		from = "self"
	} else {
		from = self.peer.Addr().String()
	}
	return fmt.Sprintf("From: %v, Key: %x; ID: %v, requestTimeout: %v, storageTimeout: %v, SData %x", from, self.Key[:4], self.Id, self.requestTimeout, self.storageTimeout, self.SData[:10])
}

/*
Root key retrieve request
Timeout in milliseconds. Note that zero timeout retrieval requests do not request forwarding, but prompt for a peers message response. therefore they also serve also as messages to retrieve peers.
MaxSize specifies the maximum size that the peer will accept. This is useful in particular if we allow storage and delivery of multichunk payload representing the entire or partial subtree unfolding from the requested root key. So when only interested in limited part of a stream (infinite trees) or only testing chunk availability etc etc, we can indicate it by limiting the size here.
In the special case that the key is identical to the peers own address (hash of NodeID) the message is to be handled as a self lookup. The response is a PeersMsg with the peers in the cademlia proximity bin corresponding to the address.
It is unclear if a retrieval request with an empty target is the same as a self lookup
*/
type retrieveRequestMsgData struct {
	Key Key
	// optional
	Id       uint64     // request id
	MaxSize  uint64     // maximum size of delivery accepted
	MaxPeers uint64     // maximum number of peers returned
	timeout  *time.Time //
	//Metadata metaData  //
	//
	peer *peer // protocol registers the requester
}

func (self retrieveRequestMsgData) String() string {
	var from string
	if self.peer == nil {
		from = "ourselves"
	} else {
		from = self.peer.Addr().String()
	}
	return fmt.Sprintf("From: %v, Key: %x; ID: %v, MaxSize: %v, MaxPeers: %s", from, self.Key[:4], self.Id, self.MaxSize, self.MaxPeers)
}

type peerAddr struct {
	IP   net.IP
	Port uint16
	ID   []byte
	n    *discover.Node
}

func (self *peerAddr) node() *discover.Node {
	if self.n == nil {
		var nodeid discover.NodeID
		copy(nodeid[:], self.ID)
		self.n = discover.NewNode(nodeid, self.IP, self.Port, self.Port)
	}
	return self.n
}

func (self *peerAddr) addr() kademlia.Address {
	return kademlia.Address(self.node().Sha())
}

func (self *peerAddr) url() string {
	return self.node().String()
}

/*
one response to retrieval, always encouraged after a retrieval request to respond with a list of peers in the same cademlia proximity bin.
The encoding of a peer is identical to that in the devp2p base protocol peers messages: [IP, Port, NodeID]
note that a node's DPA address is not the NodeID but the hash of the NodeID.
Timeout serves to indicate whether the responder is forwarding the query within the timeout or not.
The Key is the target (if response to a retrieval request) or peers address (hash of NodeID) if retrieval request was a self lookup.
It is unclear if PeersMsg with an empty Key has a special meaning or just mean the same as with the peers address as Key (cademlia bin)
*/
type peersMsgData struct {
	Peers   []*peerAddr //
	timeout *time.Time  // indicate whether responder is expected to deliver content
	Key     Key         // if a response to a retrieval request
	Id      uint64      // if a response to a retrieval request
	//
	peer *peer
}

/*
metadata is as yet a placeholder
it will likely contain info about hops or the entire forward chain of node IDs
this may allow some interesting schemes to evolve optimal routing strategies
metadata for storage and retrieval requests could specify format parameters relevant for the (blockhashing) chunking scheme used (for chunks corresponding to a treenode). For instance all runtime params for the chunker (hashing algorithm used, branching etc.)
Finally metadata can hold info relevant to some reward or compensation scheme that may be used to incentivise peers.
*/
type metaData struct{}

/*
main entrypoint, wrappers starting a server running the bzz protocol
use this constructor to attach the protocol ("class") to server caps
the Dev p2p layer then runs the protocol instance on each peer
*/
func BzzProtocol(netstore *netStore) (p2p.Protocol, error) {

	db, err := NewLDBDatabase(path.Join(netstore.path, "requests"))
	if err != nil {
		return p2p.Protocol{}, err
	}
	return p2p.Protocol{
		Name:    "bzz",
		Version: Version,
		Length:  ProtocolLength,
		Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
			return runBzzProtocol(db, netstore, p, rw)
		},
	}, nil
}

// the main loop that handles incoming messages
// note RemovePeer in the post-disconnect hook
func runBzzProtocol(db *LDBDatabase, netstore *netStore, p *p2p.Peer, rw p2p.MsgReadWriter) (err error) {

	self := &bzzProtocol{
		netStore: netstore,
		rw:       rw,
		peer:     p,
		errors: &errs.Errors{
			Package: "BZZ",
			Errors:  errorToString,
		},
		requestDb: db,
		quitC:     make(chan bool),
	}

	go self.storeRequestLoop()

	err = self.handleStatus()
	if err == nil {
		for {
			err = self.handle()
			if err != nil {
				self.netStore.hive.removePeer(peer{bzzProtocol: self})
				break
			}
		}
		close(self.quitC)
	}
	return
}

func (self *bzzProtocol) handle() error {
	msg, err := self.rw.ReadMsg()
	dpaLogger.Debugf("Incoming MSG: %v", msg)
	if err != nil {
		return err
	}
	if msg.Size > ProtocolMaxMsgSize {
		return self.protoError(ErrMsgTooLarge, "%v > %v", msg.Size, ProtocolMaxMsgSize)
	}
	// make sure that the payload has been fully consumed
	defer msg.Discard()
	/*
	   statusMsg          = iota // 0x01
	   storeRequestMsg           // 0x02
	   retrieveRequestMsg        // 0x03
	   peersMsg                  // 0x04
	*/

	switch msg.Code {
	case statusMsg:
		dpaLogger.Debugf("Status message: %v", msg)
		return self.protoError(ErrExtraStatusMsg, "")

	case storeRequestMsg:
		var req storeRequestMsgData
		if err := msg.Decode(&req); err != nil {
			return self.protoError(ErrDecode, "msg %v: %v", msg, err)
		}
		req.peer = &peer{bzzProtocol: self}
		self.netStore.addStoreRequest(&req)

	case retrieveRequestMsg:
		var req retrieveRequestMsgData
		if err := msg.Decode(&req); err != nil {
			return self.protoError(ErrDecode, "->msg %v: %v", msg, err)
		}
		dpaLogger.Debugf("Receiving retrieve request: %v", req)
		if req.Key == nil {
			return self.protoError(ErrDecode, "protocol handler: req.Key == nil || req.Timeout == nil")
		}
		req.peer = &peer{bzzProtocol: self}
		self.netStore.addRetrieveRequest(&req)

	case peersMsg:
		var req peersMsgData
		if err := msg.Decode(&req); err != nil {
			return self.protoError(ErrDecode, "->msg %v: %v", msg, err)
		}
		req.peer = &peer{bzzProtocol: self}
		self.netStore.hive.addPeerEntries(&req)

	default:
		return self.protoError(ErrInvalidMsgCode, "%v", msg.Code)
	}
	return nil
}

func (self *bzzProtocol) handleStatus() (err error) {
	// send precanned status message
	sliceNodeID := self.netStore.self.ID
	handshake := &statusMsgData{
		Version: uint64(Version),
		ID:      "honey",
		NodeID:  sliceNodeID[:],
		Addr: &peerAddr{
			ID:   sliceNodeID[:],
			IP:   self.netStore.self.IP,
			Port: self.netStore.self.TCP,
		},
		NetworkId: uint64(NetworkId),
		Caps:      []p2p.Cap{},
	}

	if err = p2p.Send(self.rw, statusMsg, handshake); err != nil {
		return err
	}

	// read and handle remote status
	var msg p2p.Msg
	msg, err = self.rw.ReadMsg()
	if err != nil {
		return err
	}

	if msg.Code != statusMsg {
		return self.protoError(ErrNoStatusMsg, "first msg has code %x (!= %x)", msg.Code, statusMsg)
	}

	if msg.Size > ProtocolMaxMsgSize {
		return self.protoError(ErrMsgTooLarge, "%v > %v", msg.Size, ProtocolMaxMsgSize)
	}

	var status statusMsgData
	if err := msg.Decode(&status); err != nil {
		return self.protoError(ErrDecode, "msg %v: %v", msg, err)
	}

	if status.NetworkId != NetworkId {
		return self.protoError(ErrNetworkIdMismatch, "%d (!= %d)", status.NetworkId, NetworkId)
	}

	if Version != status.Version {
		return self.protoError(ErrVersionMismatch, "%d (!= %d)", status.Version, Version)
	}

	glog.V(logger.Info).Infof("Peer is [bzz] capable (%d/%d)\n", status.Version, status.NetworkId)

	self.node = status.Addr.node()

	self.netStore.hive.addPeer(peer{bzzProtocol: self})

	return nil
}

// protocol instance implements kademlia.Node interface (embedded hive.peer)
func (self *bzzProtocol) Addr() (a kademlia.Address) {
	return kademlia.Address(self.node.Sha())
}

func (self *bzzProtocol) Url() string {
	return self.node.String()
}

func (self *bzzProtocol) LastActive() time.Time {
	return time.Now()
}

func (self *bzzProtocol) Drop() {
}

func (self *bzzProtocol) String() string {
	return fmt.Sprintf("%4x: %v\n", self.node.Sha().Bytes()[:4], self.Url())
}

func (self *bzzProtocol) peerAddr() *peerAddr {
	p := self.peer
	id := p.ID()
	host, port, _ := net.SplitHostPort(p.RemoteAddr().String())
	intport, _ := strconv.Atoi(port)
	return &peerAddr{
		ID:   id[:],
		IP:   net.ParseIP(host),
		Port: uint16(intport),
	}
}

// outgoing messages
func (self *bzzProtocol) retrieve(req *retrieveRequestMsgData) {
	dpaLogger.Debugf("Sending retrieve request: %v", req)
	err := p2p.Send(self.rw, retrieveRequestMsg, req)
	if err != nil {
		dpaLogger.Errorf("EncodeMsg error: %v", err)
	}
}

func (self *bzzProtocol) addrKey() []byte {
	id := self.peer.ID()
	if self.key == nil {
		self.key = Key(crypto.Sha3(id[:]))
	}
	return self.key
}

func (self *bzzProtocol) storeRequestLoop() {

	start := make([]byte, 64)
	copy(start, self.addrKey())

	key := make([]byte, 64)
	copy(key, start)
	var n int
	var it iterator.Iterator
LOOP:
	for {
		if n == 0 {
			it = self.requestDb.NewIterator()
			// dpaLogger.Debugf("seek iterator: %x", key)
			it.Seek(key)
			if !it.Valid() {
				// dpaLogger.Debugf("not valid, sleep, continue: %x", key)
				time.Sleep(1 * time.Second)
				continue
			}
			key = it.Key()
			// dpaLogger.Debugf("found db key: %x", key)
			n = 100
		}
		// dpaLogger.Debugf("checking key: %x <> %x ", key, self.key())

		// reached the end of this peers range
		if !bytes.Equal(key[:32], self.addrKey()) {
			// dpaLogger.Debugf("reached the end of this peers range: %x", key)
			n = 0
			continue
		}

		chunk, err := self.netStore.localStore.dbStore.Get(key[32:])
		if err != nil {
			self.requestDb.Delete(key)
			continue
		}
		// dpaLogger.Debugf("sending chunk: %x", chunk.Key)

		id := generateId()
		req := &storeRequestMsgData{
			Key:   chunk.Key,
			SData: chunk.SData,
			Id:    uint64(id),
		}
		self.store(req)

		n--
		self.requestDb.Delete(key)
		it.Next()
		key = it.Key()
		if len(key) == 0 {
			key = start
			if n == 0 {
				time.Sleep(1 * time.Second)
			}
			n = 0
		}

		select {
		case <-self.quitC:
			break LOOP
		default:
		}
	}
}

func (self *bzzProtocol) store(req *storeRequestMsgData) {
	p2p.Send(self.rw, storeRequestMsg, req)
}

func (self *bzzProtocol) storeRequest(key Key) {
	peerKey := make([]byte, 64)
	copy(peerKey, self.addrKey())
	copy(peerKey[32:], key[:])
	dpaLogger.Debugf("enter store request %x into db", peerKey)
	self.requestDb.Put(peerKey, []byte{0})
}

func (self *bzzProtocol) peers(req *peersMsgData) {
	p2p.Send(self.rw, peersMsg, req)
}

func (self *bzzProtocol) protoError(code int, format string, params ...interface{}) (err *errs.Error) {
	err = self.errors.New(code, format, params...)
	err.Log(glog.V(logger.Info))
	return
}

func (self *bzzProtocol) protoErrorDisconnect(err *errs.Error) {
	err.Log(glog.V(logger.Info))
	if err.Fatal() {
		self.peer.Disconnect(p2p.DiscSubprotocolError)
	}
}
