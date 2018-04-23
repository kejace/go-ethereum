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

// Package les implements the Light Ethereum Subprotocol.
package les

import (
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/kejace/go-ethereum/common"
	"github.com/kejace/go-ethereum/core/types"
	"github.com/kejace/go-ethereum/eth"
	"github.com/kejace/go-ethereum/les/flowcontrol"
	"github.com/kejace/go-ethereum/logger"
	"github.com/kejace/go-ethereum/logger/glog"
	"github.com/kejace/go-ethereum/p2p"
	"github.com/kejace/go-ethereum/rlp"
)

var (
	errClosed            = errors.New("peer set is closed")
	errAlreadyRegistered = errors.New("peer is already registered")
	errNotRegistered     = errors.New("peer is not registered")
)

const maxHeadInfoLen = 20

type peer struct {
	*p2p.Peer

	rw p2p.MsgReadWriter

	version int // Protocol version negotiated
	network int // Network ID being on

	id string

	firstHeadInfo, headInfo *announceData
	headInfoLen             int
	lock                    sync.RWMutex

	announceChn chan announceData

	fcClient       *flowcontrol.ClientNode // nil if the peer is server only
	fcServer       *flowcontrol.ServerNode // nil if the peer is client only
	fcServerParams *flowcontrol.ServerParams
	fcCosts        requestCostTable
}

func newPeer(version, network int, p *p2p.Peer, rw p2p.MsgReadWriter) *peer {
	id := p.ID()

	return &peer{
		Peer:        p,
		rw:          rw,
		version:     version,
		network:     network,
		id:          fmt.Sprintf("%x", id[:8]),
		announceChn: make(chan announceData, 20),
	}
}

// Info gathers and returns a collection of metadata known about a peer.
func (p *peer) Info() *eth.PeerInfo {
	return &eth.PeerInfo{
		Version:    p.version,
		Difficulty: p.Td(),
		Head:       fmt.Sprintf("%x", p.Head()),
	}
}

// Head retrieves a copy of the current head (most recent) hash of the peer.
func (p *peer) Head() (hash common.Hash) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	copy(hash[:], p.headInfo.Hash[:])
	return hash
}

func (p *peer) HeadAndTd() (hash common.Hash, td *big.Int) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	copy(hash[:], p.headInfo.Hash[:])
	return hash, p.headInfo.Td
}

func (p *peer) headBlockInfo() blockInfo {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return blockInfo{Hash: p.headInfo.Hash, Number: p.headInfo.Number, Td: p.headInfo.Td}
}

func (p *peer) addNotify(announce *announceData) bool {
	p.lock.Lock()
	defer p.lock.Unlock()

	if announce.Td.Cmp(p.headInfo.Td) < 1 {
		return false
	}
	if p.headInfoLen >= maxHeadInfoLen {
		//return false
		p.firstHeadInfo = p.firstHeadInfo.next
		p.headInfoLen--
	}
	if announce.haveHeaders == 0 {
		hh := p.headInfo.Number - announce.ReorgDepth
		if p.headInfo.haveHeaders < hh {
			hh = p.headInfo.haveHeaders
		}
		announce.haveHeaders = hh
	}
	p.headInfo.next = announce
	p.headInfo = announce
	p.headInfoLen++
	return true
}

func (p *peer) gotHeader(hash common.Hash, number uint64, td *big.Int) bool {
	h := p.firstHeadInfo
	ptr := 0
	for h != nil {
		if h.Hash == hash {
			if h.Number != number || h.Td.Cmp(td) != 0 {
				return false
			}
			h.headKnown = true
			h.haveHeaders = h.Number
			p.firstHeadInfo = h
			p.headInfoLen -= ptr
			last := h
			h = h.next
			// propagate haveHeaders through the chain
			for h != nil {
				hh := last.Number - h.ReorgDepth
				if last.haveHeaders < hh {
					hh = last.haveHeaders
				}
				if hh > h.haveHeaders {
					h.haveHeaders = hh
				} else {
					return true
				}
				last = h
				h = h.next
			}
			return true
		}
		h = h.next
		ptr++
	}
	return true
}

// Td retrieves the current total difficulty of a peer.
func (p *peer) Td() *big.Int {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return new(big.Int).Set(p.headInfo.Td)
}

func sendRequest(w p2p.MsgWriter, msgcode, reqID, cost uint64, data interface{}) error {
	type req struct {
		ReqID uint64
		Data  interface{}
	}
	return p2p.Send(w, msgcode, req{reqID, data})
}

func sendResponse(w p2p.MsgWriter, msgcode, reqID, bv uint64, data interface{}) error {
	type resp struct {
		ReqID, BV uint64
		Data      interface{}
	}
	return p2p.Send(w, msgcode, resp{reqID, bv, data})
}

func (p *peer) GetRequestCost(msgcode uint64, amount int) uint64 {
	cost := p.fcCosts[msgcode].baseCost + p.fcCosts[msgcode].reqCost*uint64(amount)
	if cost > p.fcServerParams.BufLimit {
		cost = p.fcServerParams.BufLimit
	}
	return cost
}

// SendAnnounce announces the availability of a number of blocks through
// a hash notification.
func (p *peer) SendAnnounce(request announceData) error {
	return p2p.Send(p.rw, AnnounceMsg, request)
}

// SendBlockHeaders sends a batch of block headers to the remote peer.
func (p *peer) SendBlockHeaders(reqID, bv uint64, headers []*types.Header) error {
	return sendResponse(p.rw, BlockHeadersMsg, reqID, bv, headers)
}

// SendBlockBodiesRLP sends a batch of block contents to the remote peer from
// an already RLP encoded format.
func (p *peer) SendBlockBodiesRLP(reqID, bv uint64, bodies []rlp.RawValue) error {
	return sendResponse(p.rw, BlockBodiesMsg, reqID, bv, bodies)
}

// SendCodeRLP sends a batch of arbitrary internal data, corresponding to the
// hashes requested.
func (p *peer) SendCode(reqID, bv uint64, data [][]byte) error {
	return sendResponse(p.rw, CodeMsg, reqID, bv, data)
}

// SendReceiptsRLP sends a batch of transaction receipts, corresponding to the
// ones requested from an already RLP encoded format.
func (p *peer) SendReceiptsRLP(reqID, bv uint64, receipts []rlp.RawValue) error {
	return sendResponse(p.rw, ReceiptsMsg, reqID, bv, receipts)
}

// SendProofs sends a batch of merkle proofs, corresponding to the ones requested.
func (p *peer) SendProofs(reqID, bv uint64, proofs proofsData) error {
	return sendResponse(p.rw, ProofsMsg, reqID, bv, proofs)
}

// SendHeaderProofs sends a batch of header proofs, corresponding to the ones requested.
func (p *peer) SendHeaderProofs(reqID, bv uint64, proofs []ChtResp) error {
	return sendResponse(p.rw, HeaderProofsMsg, reqID, bv, proofs)
}

// RequestHeadersByHash fetches a batch of blocks' headers corresponding to the
// specified header query, based on the hash of an origin block.
func (p *peer) RequestHeadersByHash(reqID, cost uint64, origin common.Hash, amount int, skip int, reverse bool) error {
	glog.V(logger.Debug).Infof("%v fetching %d headers from %x, skipping %d (reverse = %v)", p, amount, origin[:4], skip, reverse)
	return sendRequest(p.rw, GetBlockHeadersMsg, reqID, cost, &getBlockHeadersData{Origin: hashOrNumber{Hash: origin}, Amount: uint64(amount), Skip: uint64(skip), Reverse: reverse})
}

// RequestHeadersByNumber fetches a batch of blocks' headers corresponding to the
// specified header query, based on the number of an origin block.
func (p *peer) RequestHeadersByNumber(reqID, cost, origin uint64, amount int, skip int, reverse bool) error {
	glog.V(logger.Debug).Infof("%v fetching %d headers from #%d, skipping %d (reverse = %v)", p, amount, origin, skip, reverse)
	return sendRequest(p.rw, GetBlockHeadersMsg, reqID, cost, &getBlockHeadersData{Origin: hashOrNumber{Number: origin}, Amount: uint64(amount), Skip: uint64(skip), Reverse: reverse})
}

// RequestBodies fetches a batch of blocks' bodies corresponding to the hashes
// specified.
func (p *peer) RequestBodies(reqID, cost uint64, hashes []common.Hash) error {
	glog.V(logger.Debug).Infof("%v fetching %d block bodies", p, len(hashes))
	return sendRequest(p.rw, GetBlockBodiesMsg, reqID, cost, hashes)
}

// RequestCode fetches a batch of arbitrary data from a node's known state
// data, corresponding to the specified hashes.
func (p *peer) RequestCode(reqID, cost uint64, reqs []*CodeReq) error {
	glog.V(logger.Debug).Infof("%v fetching %v state data", p, len(reqs))
	return sendRequest(p.rw, GetCodeMsg, reqID, cost, reqs)
}

// RequestReceipts fetches a batch of transaction receipts from a remote node.
func (p *peer) RequestReceipts(reqID, cost uint64, hashes []common.Hash) error {
	glog.V(logger.Debug).Infof("%v fetching %v receipts", p, len(hashes))
	return sendRequest(p.rw, GetReceiptsMsg, reqID, cost, hashes)
}

// RequestProofs fetches a batch of merkle proofs from a remote node.
func (p *peer) RequestProofs(reqID, cost uint64, reqs []*ProofReq) error {
	glog.V(logger.Debug).Infof("%v fetching %v proofs", p, len(reqs))
	return sendRequest(p.rw, GetProofsMsg, reqID, cost, reqs)
}

// RequestHeaderProofs fetches a batch of header merkle proofs from a remote node.
func (p *peer) RequestHeaderProofs(reqID, cost uint64, reqs []*ChtReq) error {
	glog.V(logger.Debug).Infof("%v fetching %v header proofs", p, len(reqs))
	return sendRequest(p.rw, GetHeaderProofsMsg, reqID, cost, reqs)
}

func (p *peer) SendTxs(cost uint64, txs types.Transactions) error {
	glog.V(logger.Debug).Infof("%v relaying %v txs", p, len(txs))
	p.fcServer.SendRequest(0, cost)
	return p2p.Send(p.rw, SendTxMsg, txs)
}

type keyValueEntry struct {
	Key   string
	Value rlp.RawValue
}
type keyValueList []keyValueEntry
type keyValueMap map[string]rlp.RawValue

func (l keyValueList) add(key string, val interface{}) keyValueList {
	var entry keyValueEntry
	entry.Key = key
	if val == nil {
		val = uint64(0)
	}
	enc, err := rlp.EncodeToBytes(val)
	if err == nil {
		entry.Value = enc
	}
	return append(l, entry)
}

func (l keyValueList) decode() keyValueMap {
	m := make(keyValueMap)
	for _, entry := range l {
		m[entry.Key] = entry.Value
	}
	return m
}

func (m keyValueMap) get(key string, val interface{}) error {
	enc, ok := m[key]
	if !ok {
		return errResp(ErrHandshakeMissingKey, "%s", key)
	}
	if val == nil {
		return nil
	}
	return rlp.DecodeBytes(enc, val)
}

func (p *peer) sendReceiveHandshake(sendList keyValueList) (keyValueList, error) {
	// Send out own handshake in a new thread
	errc := make(chan error, 1)
	go func() {
		errc <- p2p.Send(p.rw, StatusMsg, sendList)
	}()
	// In the mean time retrieve the remote status message
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return nil, err
	}
	if msg.Code != StatusMsg {
		return nil, errResp(ErrNoStatusMsg, "first msg has code %x (!= %x)", msg.Code, StatusMsg)
	}
	if msg.Size > ProtocolMaxMsgSize {
		return nil, errResp(ErrMsgTooLarge, "%v > %v", msg.Size, ProtocolMaxMsgSize)
	}
	// Decode the handshake
	var recvList keyValueList
	if err := msg.Decode(&recvList); err != nil {
		return nil, errResp(ErrDecode, "msg %v: %v", msg, err)
	}
	if err := <-errc; err != nil {
		return nil, err
	}
	return recvList, nil
}

// Handshake executes the les protocol handshake, negotiating version number,
// network IDs, difficulties, head and genesis blocks.
func (p *peer) Handshake(td *big.Int, head common.Hash, headNum uint64, genesis common.Hash, server *LesServer) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	var send keyValueList
	send = send.add("protocolVersion", uint64(p.version))
	send = send.add("networkId", uint64(p.network))
	send = send.add("headTd", td)
	send = send.add("headHash", head)
	send = send.add("headNum", headNum)
	send = send.add("genesisHash", genesis)
	if server != nil {
		send = send.add("serveHeaders", nil)
		send = send.add("serveChainSince", uint64(0))
		send = send.add("serveStateSince", uint64(0))
		send = send.add("txRelay", nil)
		send = send.add("flowControl/BL", server.defParams.BufLimit)
		send = send.add("flowControl/MRR", server.defParams.MinRecharge)
		list := server.fcCostStats.getCurrentList()
		send = send.add("flowControl/MRC", list)
		p.fcCosts = list.decode()
	}
	recvList, err := p.sendReceiveHandshake(send)
	if err != nil {
		return err
	}
	recv := recvList.decode()

	var rGenesis, rHash common.Hash
	var rVersion, rNetwork, rNum uint64
	var rTd *big.Int

	if err := recv.get("protocolVersion", &rVersion); err != nil {
		return err
	}
	if err := recv.get("networkId", &rNetwork); err != nil {
		return err
	}
	if err := recv.get("headTd", &rTd); err != nil {
		return err
	}
	if err := recv.get("headHash", &rHash); err != nil {
		return err
	}
	if err := recv.get("headNum", &rNum); err != nil {
		return err
	}
	if err := recv.get("genesisHash", &rGenesis); err != nil {
		return err
	}

	if rGenesis != genesis {
		return errResp(ErrGenesisBlockMismatch, "%x (!= %x)", rGenesis, genesis)
	}
	if int(rNetwork) != p.network {
		return errResp(ErrNetworkIdMismatch, "%d (!= %d)", rNetwork, p.network)
	}
	if int(rVersion) != p.version {
		return errResp(ErrProtocolVersionMismatch, "%d (!= %d)", rVersion, p.version)
	}
	if server != nil {
		if recv.get("serveStateSince", nil) == nil {
			return errResp(ErrUselessPeer, "wanted client, got server")
		}
		p.fcClient = flowcontrol.NewClientNode(server.fcManager, server.defParams)
	} else {
		if recv.get("serveChainSince", nil) != nil {
			return errResp(ErrUselessPeer, "peer cannot serve chain")
		}
		if recv.get("serveStateSince", nil) != nil {
			return errResp(ErrUselessPeer, "peer cannot serve state")
		}
		if recv.get("txRelay", nil) != nil {
			return errResp(ErrUselessPeer, "peer cannot relay transactions")
		}
		params := &flowcontrol.ServerParams{}
		if err := recv.get("flowControl/BL", &params.BufLimit); err != nil {
			return err
		}
		if err := recv.get("flowControl/MRR", &params.MinRecharge); err != nil {
			return err
		}
		var MRC RequestCostList
		if err := recv.get("flowControl/MRC", &MRC); err != nil {
			return err
		}
		p.fcServerParams = params
		p.fcServer = flowcontrol.NewServerNode(params)
		p.fcCosts = MRC.decode()
	}

	p.firstHeadInfo = &announceData{Td: rTd, Hash: rHash, Number: rNum}
	p.headInfo = p.firstHeadInfo
	p.headInfoLen = 1
	return nil
}

// String implements fmt.Stringer.
func (p *peer) String() string {
	return fmt.Sprintf("Peer %s [%s]", p.id,
		fmt.Sprintf("les/%d", p.version),
	)
}

// peerSet represents the collection of active peers currently participating in
// the Light Ethereum sub-protocol.
type peerSet struct {
	peers  map[string]*peer
	lock   sync.RWMutex
	closed bool
}

// newPeerSet creates a new peer set to track the active participants.
func newPeerSet() *peerSet {
	return &peerSet{
		peers: make(map[string]*peer),
	}
}

// Register injects a new peer into the working set, or returns an error if the
// peer is already known.
func (ps *peerSet) Register(p *peer) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if ps.closed {
		return errClosed
	}
	if _, ok := ps.peers[p.id]; ok {
		return errAlreadyRegistered
	}
	ps.peers[p.id] = p
	return nil
}

// Unregister removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (ps *peerSet) Unregister(id string) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if _, ok := ps.peers[id]; !ok {
		return errNotRegistered
	}
	delete(ps.peers, id)
	return nil
}

// AllPeerIDs returns a list of all registered peer IDs
func (ps *peerSet) AllPeerIDs() []string {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	res := make([]string, len(ps.peers))
	idx := 0
	for id, _ := range ps.peers {
		res[idx] = id
		idx++
	}
	return res
}

// Peer retrieves the registered peer with the given id.
func (ps *peerSet) Peer(id string) *peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.peers[id]
}

// Len returns if the current number of peers in the set.
func (ps *peerSet) Len() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return len(ps.peers)
}

// BestPeer retrieves the known peer with the currently highest total difficulty.
func (ps *peerSet) BestPeer() *peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	var (
		bestPeer *peer
		bestTd   *big.Int
	)
	for _, p := range ps.peers {
		if td := p.Td(); bestPeer == nil || td.Cmp(bestTd) > 0 {
			bestPeer, bestTd = p, td
		}
	}
	return bestPeer
}

// AllPeers returns all peers in a list
func (ps *peerSet) AllPeers() []*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*peer, len(ps.peers))
	i := 0
	for _, peer := range ps.peers {
		list[i] = peer
		i++
	}
	return list
}

// Close disconnects all peers.
// No new peers can be registered after Close has returned.
func (ps *peerSet) Close() {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	for _, p := range ps.peers {
		p.Disconnect(p2p.DiscQuitting)
	}
	ps.closed = true
}
