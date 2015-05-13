package kademlia

import (
	"fmt"
	"sort"
	// "math"
	"encoding/json"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/logger"
)

var kadlogger = logger.NewLogger("KΛÐ")

const (
	bucketSize = 20
	maxProx    = 255
)

type Kademlia struct {
	// immutable baseparam
	addr Address

	// adjustable parameters
	MaxProx              int
	MaxProxBinSize       int
	BucketSize           int
	currentMaxBucketSize int
	nodeDB               [][]*NodeRecord
	nodeIndex            map[Address]*NodeRecord

	GetNode func(int)

	// state
	proxLimit int
	proxSize  int

	//
	count   int
	buckets []*bucket

	dblock sync.RWMutex
	lock   sync.RWMutex
	quitC  chan bool
}

type Address common.Hash

type Node interface {
	Addr() Address
	Url() string
	LastActive() time.Time
	Drop()
}

type NodeRecord struct {
	Address Address `json:address`
	Url     string  `json:url`
	Active  int64   `json:active`
	node    Node
}

func (self *NodeRecord) setActive() {
	if self.node != nil {
		self.Active = self.node.LastActive().UnixNano()
	}
}

type kadDB struct {
	Address Address         `json:address`
	Nodes   [][]*NodeRecord `json:nodes`
}

// public constructor
// hash is a byte slice of length equal to self.HashBytes
func New() *Kademlia {
	return &Kademlia{}
}

// accessor for KAD self address
func (self *Kademlia) Addr() Address {
	return self.addr
}

// accessor for KAD self count
func (self *Kademlia) Count() int {
	return self.count
}

// Start brings up a pool of entries potentially from an offline persisted source
// and sets default values for optional parameters
func (self *Kademlia) Start(addr Address) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.quitC != nil {
		return nil
	}
	self.addr = addr
	if self.MaxProx == 0 {
		self.MaxProx = maxProx
	}
	if self.BucketSize == 0 {
		self.BucketSize = bucketSize
	}
	// runtime parameters
	if self.MaxProxBinSize == 0 {
		self.MaxProxBinSize = self.BucketSize
	}

	self.buckets = make([]*bucket, self.MaxProx+1)
	for i, _ := range self.buckets {
		self.buckets[i] = &bucket{size: self.BucketSize} // will initialise bucket{int(0),[]Node(nil),sync.Mutex}
	}

	self.nodeDB = make([][]*NodeRecord, 8*len(self.addr))
	self.nodeIndex = make(map[Address]*NodeRecord)

	self.quitC = make(chan bool)
	return nil
}

// Stop saves the routing table into a persistant form
func (self *Kademlia) Stop(path string) (err error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.quitC == nil {
		return
	}
	close(self.quitC)
	self.quitC = nil

	if len(path) > 0 {
		err = self.Save(path)
		if err != nil {
			kadlogger.Warnf("unable to save node records: %v", err)
		}
	}
	return
}

// RemoveNode is the entrypoint where nodes are taken offline
func (self *Kademlia) RemoveNode(node Node) (err error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	index := self.proximityBin(node.Addr())
	bucket := self.buckets[index]
	for i := 0; i < len(bucket.nodes); i++ {
		if node.Addr() == bucket.nodes[i].Addr() {
			bucket.nodes = append(bucket.nodes[:i], bucket.nodes[(i+1):]...)
		}
	}
	self.count--
	if len(bucket.nodes) < bucket.size {
		err = fmt.Errorf("insufficient nodes (%v) in bucket %v", len(bucket.nodes), index)
	}
	if len(bucket.nodes) == 0 {
		self.adjustProx(index, -1)
	}
	// async callback to notify user that bucket needs filling
	// action is left to the user
	if self.GetNode != nil {
		go self.GetNode(index)
	}
	return
}

// AddNode is the entry point where new nodes are registered
func (self *Kademlia) AddNode(node Node) (err error) {

	self.lock.Lock()
	defer self.lock.Unlock()

	index := self.proximityBin(node.Addr())
	kadlogger.Debugf("bin %d, len: %d\n", index, len(self.buckets))

	bucket := self.buckets[index]
	err = bucket.insert(node)
	if err != nil {
		return
	}
	self.count++
	if index >= self.proxLimit {
		self.adjustProx(index, 1)
	}

	go func() {
		self.dblock.Lock()
		defer self.dblock.Unlock()
		record, found := self.nodeIndex[node.Addr()]
		if found {
			record.node = node
		} else {
			record = &NodeRecord{
				Address: node.Addr(),
				Url:     node.Url(),
				Active:  node.LastActive().UnixNano(),
				node:    node,
			}
			self.nodeIndex[node.Addr()] = record
			self.nodeDB[index] = append(self.nodeDB[index], record)
		}
	}()

	kadlogger.Infof("add peer %v...", node)
	return

}

// adjust Prox (proxLimit and proxSize after an insertion of add nodes into bucket r)
func (self *Kademlia) adjustProx(r int, add int) {
	switch {
	case add > 0 && r == self.proxLimit:
		self.proxLimit += add
		for ; self.proxLimit < self.MaxProx && len(self.buckets[self.proxLimit].nodes) > 0; self.proxLimit++ {
			self.proxSize -= len(self.buckets[self.proxLimit].nodes)
		}
	case add > 0 && r > self.proxLimit && self.proxSize+add > self.MaxProxBinSize:
		self.proxLimit++
		self.proxSize -= len(self.buckets[r].nodes) - add
	case add > 0 && r > self.proxLimit:
		self.proxSize += add
	case add < 0 && r < self.proxLimit && len(self.buckets[r].nodes) == 0:
		for i := self.proxLimit - 1; i > r; i-- {
			self.proxSize += len(self.buckets[i].nodes)
		}
		self.proxLimit = r
	}
}

/*
GetNodes(target) returns the list of nodes belonging to the same proximity bin
as the target. The most proximate bin will be the union of the bins between
proxLimit and MaxProx. proxLimit is dynamically adjusted so that 1) there is no
empty buckets in bin < proxLimit and 2) the sum of all items are the maximum
possible but lower than MaxProxBinSize
*/
func (self *Kademlia) GetNodes(target Address, max int) []Node {
	return self.getNodes(target, max).nodes
}

func (self *Kademlia) getNodes(target Address, max int) (r nodesByDistance) {
	self.lock.RLock()
	defer self.lock.RUnlock()
	r.target = target
	index := self.proximityBin(target)
	start := index
	var down bool
	if index >= self.proxLimit {
		index = self.proxLimit
		start = self.MaxProx
		down = true
	}
	var n int
	limit := max
	if max == 0 {
		limit = 1000
	}
	for {
		bucket := self.buckets[start].nodes
		for i := 0; i < len(bucket); i++ {
			r.push(bucket[i], limit)
			n++
		}
		if max == 0 && start <= index && (n > 0 || start == 0) ||
			max > 0 && down && start <= index && (n >= limit || n == self.Count() || start == 0) {
			break
		}
		if down {
			start--
		} else {
			if start == self.MaxProx {
				if index == 0 {
					break
				}
				start = index - 1
				down = true
			} else {
				start++
			}
		}
	}
	return
}

// this is used to add node records to the persisted db
// TODO: maybe db needs to be purged occasionally (reputation will take care of
// that)
func (self *Kademlia) AddNodeRecords(nrs []*NodeRecord) {
	self.dblock.Lock()
	defer self.dblock.Unlock()
	for _, node := range nrs {
		_, found := self.nodeIndex[node.Address]
		if !found {
			self.nodeIndex[node.Address] = node
			index := self.proximityBin(node.Address)
			self.nodeDB[index] = append(self.nodeDB[index], node)
		}
	}
}

/*
GetNodeRecords gives back an at most max length slice of node records
in order of decreasing priority for desired connection
Used to pick candidates for live nodes to satisfy Kademlia network for Swarm

Does a round robin on buckets starting from 0 to proxLimit then back
on each round i we inspect if live-nodes fill the bucket
if len(nodes) + i < currentMaxBucketSize, then take ith element in corresponding
db row ordered by reputation (active time?)

This has double role. Starting as naive node with empty db, this implements
Kademlia bootstrapping
As a mature node, it manages quickly fill in blanks or short lines
All on demand
*/
func (self *Kademlia) GetNodeRecords(max int) (nrs []*NodeRecord, err error) {
	var round int
	for max > 0 {
		for i, b := range self.buckets {
			if len(b.nodes)+round < self.currentMaxBucketSize {
				if nr := self.getNodeRecord(i, round); nr != nil {
					nrs = append(nrs)
				}
			}
		}
		round++
		max--
	}
	return
}

func (self *Kademlia) getNodeRecord(row, col int) (nr *NodeRecord) {
	if row >= 0 && row < len(self.nodeDB) &&
		col >= 0 && col < len(self.nodeDB[row]) {
		nr = self.nodeDB[row][col]
	}
	return
}

// in situ mutable bucket
type bucket struct {
	size  int
	nodes []Node
	lock  sync.RWMutex
}

func (a Address) Bin() string {
	var bs []string
	for _, b := range a[:] {
		bs = append(bs, fmt.Sprintf("%08b", b))
	}
	return strings.Join(bs, "")
}

// nodesByDistance is a list of nodes, ordered by distance to target.
type nodesByDistance struct {
	nodes  []Node
	target Address
}

func sortedByDistanceTo(target Address, slice []Node) bool {
	var last Address
	for i, node := range slice {
		if i > 0 {
			if proxCmp(target, node.Addr(), last) < 0 {
				return false
			}
		}
		last = node.Addr()
	}
	return true
}

// push(node, max) adds the given node to the list, keeping the total size
// below max elements.
func (h *nodesByDistance) push(node Node, max int) {
	// returns the firt index ix such that func(i) returns true
	ix := sort.Search(len(h.nodes), func(i int) bool {
		return proxCmp(h.target, h.nodes[i].Addr(), node.Addr()) >= 0
	})

	if len(h.nodes) < max {
		h.nodes = append(h.nodes, node)
	}
	if ix < len(h.nodes) {
		copy(h.nodes[ix+1:], h.nodes[ix:])
		h.nodes[ix] = node
	}
}

// insert adds a peer to a bucket either by appending to existing items if
// bucket length does not exceed bucketLength, or by replacing the worst
// Node in the bucket
func (self *bucket) insert(node Node) (err error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if len(self.nodes) >= self.size { // >= allows us to add peers beyond the bucketsize limitation
		worst := self.worstNode()
		self.nodes[worst] = node
	} else {
		self.nodes = append(self.nodes, node)
	}
	return
}

// worst expunges the single worst entry in a row, where worst entry is with a peer that has not been active the longests
func (self *bucket) worstNode() (index int) {
	var oldest time.Time
	for i, node := range self.nodes {
		if (oldest == time.Time{}) || node.LastActive().Before(oldest) {
			oldest = node.LastActive()
			index = i
		}
	}
	return
}

/*
Taking the proximity value relative to a fix point x classifies the points in
the space (n byte long byte sequences) into bins the items in which are each at
most half as distant from x as items in the previous bin. Given a sample of
uniformly distrbuted items (a hash function over arbitrary sequence) the
proximity scale maps onto series of subsets with cardinalities on a negative
exponential scale.

It also has the property that any two item belonging to the same bin are at
most half as distant from each other as they are from x.

If we think of random sample of items in the bins as connections in a network of interconnected nodes than relative proximity can serve as the basis for local
decisions for graph traversal where the task is to find a route between two
points. Since in every step of forwarding, the finite distance halves, there is
a guaranteed constant maximum limit on the number of hops needed to reach one
node from the other.
*/

func (self *Kademlia) proximityBin(other Address) (ret int) {
	ret = proximity(self.addr, other)
	if ret > self.MaxProx {
		ret = self.MaxProx
	}
	return
}

/*
The distance metric MSB(x, y) of two equal length byte sequences x an y is the
value of the binary integer cast of the xor-ed byte sequence (most significant
bit first).
proximity(x, y) counts the common zeros in the front of this distance measure.
*/
func proximity(one, other Address) (ret int) {
	for i := 0; i < len(one); i++ {
		oxo := one[i] ^ other[i]
		for j := 0; j < 8; j++ {
			if (uint8(oxo)>>uint8(7-j))&0x1 != 0 {
				return i*8 + j
			}
		}
	}
	return len(one) * 8
}

// proxCmp compares the distances a->target and b->target.
// Returns -1 if a is closer to target, 1 if b is closer to target
// and 0 if they are equal.
func proxCmp(target, a, b Address) int {
	for i := range target {
		da := a[i] ^ target[i]
		db := b[i] ^ target[i]
		if da > db {
			return 1
		} else if da < db {
			return -1
		}
	}
	return 0
}

func (self *Kademlia) DB() [][]*NodeRecord {
	return self.nodeDB
}

func (n *NodeRecord) bumpActive() {
	stamp := time.Now().Unix()
	atomic.StoreInt64(&n.Active, stamp)
}

func (n *NodeRecord) LastActive() time.Time {
	stamp := atomic.LoadInt64(&n.Active)
	return time.Unix(stamp, 0)
}

// save persists all peers encountered
func (self *Kademlia) Save(path string) error {

	kad := kadDB{
		Address: self.addr,
		Nodes:   self.nodeDB,
	}

	for _, b := range kad.Nodes {
		for _, node := range b {
			node.setActive()
		}
	}

	data, err := json.MarshalIndent(&kad, "", " ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(path, data, os.ModePerm)
}

// loading the idle node record from disk
func (self *Kademlia) Load(path string) (err error) {
	var data []byte
	data, err = ioutil.ReadFile(path)
	if err != nil {
		return
	}
	var kad kadDB
	err = json.Unmarshal(data, &kad)
	if err != nil {
		return
	}
	self.nodeDB = kad.Nodes
	if self.addr != kad.Address {
		return fmt.Errorf("invalid kad db: address mismatch, expected %v, got %v", self.addr, kad.Address)
	}
	return
}