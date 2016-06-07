// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package discover

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// In this test, nodes try to randomly resolve each other.
func TestSimRandomResolve(t *testing.T) {
	t.Skip("boring")
	if runWithPlaygroundTime(t) {
		return
	}

	sim := newSimulation()
	bootnode := sim.launchNode()

	// A new node joins every 10s.
	launcher := time.NewTicker(10 * time.Second)
	go func() {
		for range launcher.C {
			net := sim.launchNode()
			go randomResolves(t, sim, net)
			if err := net.SetFallbackNodes([]*Node{bootnode.Self()}); err != nil {
				panic(err)
			}
			fmt.Printf("launched @ %v: %x\n", time.Now(), net.Self().ID[:16])
		}
	}()

	time.Sleep(3 * time.Hour)
	launcher.Stop()
	sim.shutdown()
	sim.printStats()
}

func TestSimTopics(t *testing.T) {
	if runWithPlaygroundTime(t) {
		return
	}

	// glog.SetV(6)
	// glog.SetToStderr(true)

	sim := newSimulation()
	bootnode := sim.launchNode()

	// A new node joins every 10s.
	launcher := time.NewTicker(10 * time.Second)
	go func() {
		for range launcher.C {
			net := sim.launchNode()
			go net.RegisterTopic("foo", nil)
			if err := net.SetFallbackNodes([]*Node{bootnode.Self()}); err != nil {
				panic(err)
			}
			fmt.Printf("launched @ %v: %x\n", time.Now(), net.Self().ID[:16])
		}
	}()

	time.Sleep(3 * time.Hour)
	launcher.Stop()
	sim.shutdown()
	sim.printStats()
}

func randomResolves(t *testing.T, s *simulation, net *Network) {
	randtime := func() time.Duration {
		return time.Duration(rand.Intn(50)+20) * time.Second
	}
	lookup := func(target NodeID) bool {
		result := net.Resolve(target)
		return result != nil && result.ID == target
	}

	timer := time.NewTimer(randtime())
	for {
		select {
		case <-timer.C:
			target := s.randomNode().Self().ID
			if !lookup(target) {
				t.Errorf("node %x: target %x not found", net.Self().ID[:8], target[:8])
			}
			timer.Reset(randtime())
		case <-net.closed:
			return
		}
	}
}

type simulation struct {
	mu      sync.RWMutex
	nodes   map[NodeID]*Network
	nodectr uint32
}

func newSimulation() *simulation {
	return &simulation{nodes: make(map[NodeID]*Network)}
}

func (s *simulation) shutdown() {
	s.mu.RLock()
	alive := make([]*Network, 0, len(s.nodes))
	for _, n := range s.nodes {
		alive = append(alive, n)
	}
	defer s.mu.RUnlock()

	for _, n := range alive {
		n.Close()
	}
}

func (s *simulation) printStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Println("node counter:", s.nodectr)
	fmt.Println("alive nodes:", len(s.nodes))

	// for _, n := range s.nodes {
	// 	fmt.Printf("%x\n", n.tab.self.ID[:8])
	// 	transport := n.conn.(*simTransport)
	// 	fmt.Println("   joined:", transport.joinTime)
	// 	fmt.Println("   sends:", transport.hashctr)
	// 	fmt.Println("   table size:", n.tab.count)
	// }
}

func (s *simulation) randomNode() *Network {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := rand.Intn(len(s.nodes))
	for _, net := range s.nodes {
		if n == 0 {
			return net
		}
		n--
	}
	return nil
}

func (s *simulation) launchNode() *Network {
	var (
		num = s.nodectr
		key = newkey()
		id  = PubkeyID(&key.PublicKey)
		ip  = make(net.IP, 4)
	)
	s.nodectr++
	binary.BigEndian.PutUint32(ip, num)
	ip[0] = 10
	addr := &net.UDPAddr{IP: ip, Port: 30303}

	transport := &simTransport{joinTime: time.Now(), sender: id, senderAddr: addr, sim: s}
	net, err := newNetwork(transport, key.PublicKey, nil, "<no database>")
	if err != nil {
		panic("cannot launch new node: " + err.Error())
	}
	s.mu.Lock()
	s.nodes[id] = net
	s.mu.Unlock()

	return net
}

func (s *simulation) dropNode(id NodeID) {
	s.mu.Lock()
	n := s.nodes[id]
	delete(s.nodes, id)
	s.mu.Unlock()

	n.Close()
}

type simTransport struct {
	joinTime   time.Time
	sender     NodeID
	senderAddr *net.UDPAddr
	sim        *simulation
	hashctr    uint64
}

func (st *simTransport) localAddr() *net.UDPAddr {
	return st.senderAddr
}

func (st *simTransport) Close() {}

func (st *simTransport) send(remote *Node, ptype nodeEvent, data interface{}) {
	hash := st.nextHash()
	st.sendPacket(remote.ID, ingressPacket{
		remoteID:   st.sender,
		remoteAddr: st.senderAddr,
		hash:       hash,
		ev:         ptype,
		data:       data,
	})
}

func (st *simTransport) sendPing(remote *Node, remoteAddr *net.UDPAddr, topics []Topic) []byte {
	hash := st.nextHash()
	st.sendPacket(remote.ID, ingressPacket{
		remoteID:   st.sender,
		remoteAddr: st.senderAddr,
		hash:       hash,
		ev:         pingPacket,
		data: &ping{
			Version:    4,
			From:       rpcEndpoint{IP: st.senderAddr.IP, UDP: uint16(st.senderAddr.Port), TCP: 30303},
			To:         rpcEndpoint{IP: remoteAddr.IP, UDP: uint16(remoteAddr.Port), TCP: 30303},
			Expiration: uint64(time.Now().Unix() + int64(expiration)),
			Topics:     topics,
		},
	})
	return hash
}

func (st *simTransport) sendPong(remote *Node, pingHash []byte) {
	raddr := remote.addr()
	st.sendPacket(remote.ID, ingressPacket{
		remoteID:   st.sender,
		remoteAddr: st.senderAddr,
		hash:       st.nextHash(),
		ev:         pongPacket,
		data: &pong{
			To:         rpcEndpoint{IP: raddr.IP, UDP: uint16(raddr.Port), TCP: 30303},
			ReplyTok:   pingHash,
			Expiration: uint64(time.Now().Unix() + int64(expiration)),
		},
	})
}

func (st *simTransport) sendFindnodeHash(remote *Node, target common.Hash) {
	st.sendPacket(remote.ID, ingressPacket{
		remoteID:   st.sender,
		remoteAddr: st.senderAddr,
		hash:       st.nextHash(),
		ev:         findnodeHashPacket,
		data: &findnodeHash{
			Target:     target,
			Expiration: uint64(time.Now().Unix() + int64(expiration)),
		},
	})
}

func (st *simTransport) sendTopicRegister(remote *Node, topics []Topic, pong []byte) {
	st.sendPacket(remote.ID, ingressPacket{
		remoteID:   st.sender,
		remoteAddr: st.senderAddr,
		hash:       st.nextHash(),
		ev:         topicRegisterPacket,
		data: &topicRegister{
			Topics: topics,
			Pong:   pong,
		},
	})
}

func (st *simTransport) sendTopicNodes(remote *Node, queryHash common.Hash, nodes []*Node) {
	rnodes := make([]rpcNode, len(nodes))
	for i := range nodes {
		rnodes[i] = nodeToRPC(nodes[i])
	}
	st.sendPacket(remote.ID, ingressPacket{
		remoteID:   st.sender,
		remoteAddr: st.senderAddr,
		hash:       st.nextHash(),
		ev:         topicNodesPacket,
		data:       &topicNodes{Echo: queryHash, Nodes: rnodes},
	})
}

func (st *simTransport) sendNeighbours(remote *Node, nodes []*Node) {
	// TODO: send multiple packets
	rnodes := make([]rpcNode, len(nodes))
	for i := range nodes {
		rnodes[i] = nodeToRPC(nodes[i])
	}
	st.sendPacket(remote.ID, ingressPacket{
		remoteID:   st.sender,
		remoteAddr: st.senderAddr,
		hash:       st.nextHash(),
		ev:         neighborsPacket,
		data: &neighbors{
			Nodes:      rnodes,
			Expiration: uint64(time.Now().Unix() + int64(expiration)),
		},
	})
}

func (st *simTransport) nextHash() []byte {
	v := atomic.AddUint64(&st.hashctr, 1)
	var hash common.Hash
	binary.BigEndian.PutUint64(hash[:], v)
	return hash[:]
}

func (st *simTransport) sendPacket(remote NodeID, p ingressPacket) {
	st.sim.mu.RLock()
	recipient := st.sim.nodes[remote]
	st.sim.mu.RUnlock()

	// TODO: apply packet loss
	time.AfterFunc(200*time.Millisecond, func() {
		recipient.reqReadPacket(p)
	})
}
