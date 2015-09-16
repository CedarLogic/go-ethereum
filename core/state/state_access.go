// Copyright 2014 The go-ethereum Authors
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

// Package state provides a caching layer atop the Ethereum state trie.
package state

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/access"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/trie"
)

var nullAddress = common.Address{}

type TrieAccess struct {
	ca     *access.ChainAccess
	root   common.Hash
	trieDb trie.Database
}

func NewTrieAccess(ca *access.ChainAccess, root common.Hash, trieDb trie.Database) *TrieAccess {
	return &TrieAccess{
		ca:     ca,
		root:   root,
		trieDb: trieDb,
	}
}

func (self *TrieAccess) RetrieveKey(key []byte) bool {
	r := &TrieEntryAccess{root: self.root, trieDb: self.trieDb, key: key}
	return self.ca.Retrieve(r, true) == nil
}

type TrieEntryAccess struct {
	root       common.Hash
	trieDb     trie.Database
	address    common.Address // if nullAddress, it's the account trie
	key, value []byte
	proof      trie.MerkleProof
	skipLevels int // set by DbGet() if unsuccessful
}

func (self *TrieEntryAccess) Request(peer *access.Peer) error {
	req := &access.ProofReq{
		Root: self.root,
		Key:  self.key,
	}
	return peer.GetProof([]*access.ProofReq{req})
}

func (self *TrieEntryAccess) Valid(msg *access.Msg) bool {

	if msg.MsgType != access.MsgProof {
		return false
	}
	proof := msg.Obj.(trie.MerkleProof)
	value, err := trie.VerifyProof(self.root, self.key, proof)
	if err == nil {
		self.proof = proof
		self.value = value
		return true
	}
	return false
}

func (self *TrieEntryAccess) DbGet() bool {
	return false // not used
}

func (self *TrieEntryAccess) DbPut() {
	trie.StoreProof(self.trieDb, self.proof)
}

type NodeDataAccess struct {
	db   ethdb.Database
	hash common.Hash
	data []byte
}

func (self *NodeDataAccess) Request(peer *access.Peer) error {
	return peer.GetNodeData([]common.Hash{self.hash})
}

func (self *NodeDataAccess) Valid(msg *access.Msg) bool {
	if msg.MsgType != access.MsgNodeData {
		return false
	}
	reply := msg.Obj.([][]byte)
	if len(reply) != 1 {
		return false
	}
	data := reply[0]
	hash := crypto.Sha3Hash(data)
	valid := bytes.Compare(self.hash[:], hash[:]) == 0
	if valid {
		self.data = data
	}
	return valid
}

func (self *NodeDataAccess) DbGet() bool {
	data, _ := self.db.Get(self.hash[:])
	if len(data) == 0 {
		return false
	}
	self.data = data
	return true
}

func (self *NodeDataAccess) DbPut() {
	self.db.Put(self.hash[:], self.data)
}

func RetrieveNodeData(ca *access.ChainAccess, hash common.Hash) []byte {
	r := &NodeDataAccess{db: ca.Db(), hash: hash}
	ca.Retrieve(r, true)
	return r.data
}