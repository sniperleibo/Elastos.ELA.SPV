package spvwallet

import (
	"errors"
	"fmt"
	"time"
	"strings"

	"github.com/elastos/Elastos.ELA.SPV/bloom"
	tx "github.com/elastos/Elastos.ELA.SPV/core/transaction"
	"github.com/elastos/Elastos.ELA.SPV/p2p"
	"github.com/elastos/Elastos.ELA.SPV/sdk"
	"github.com/elastos/Elastos.ELA.SPV/spvwallet/config"
	"github.com/elastos/Elastos.ELA.SPV/spvwallet/log"
	"github.com/elastos/Elastos.ELA.SPV/spvwallet/msg"
)

var spvWallet *SPVWallet

func InitSPV(clientId uint64) (*SPVWallet, error) {
	var err error
	spvWallet = new(SPVWallet)
	spvWallet.clientId = clientId
	spvWallet.chain, err = NewBlockchain()
	if err != nil {
		return nil, err
	}
	spvWallet.chain.OnTxCommit = OnTxCommit
	spvWallet.chain.OnBlockCommit = OnBlockCommit
	spvWallet.chain.OnRollback = OnRollback
	spvWallet.SyncManager = NewSyncManager()

	// Set Magic number of the P2P network
	p2p.Magic = sdk.MainNetMagic
	// Convert seed addresses to SPVServerPort according to the SPV protocol
	seeds := toSPVAddr(config.Values().SeedList)
	// Create peer manager of the P2P network
	spvWallet.pm = p2p.InitPeerManager(spvWallet.initLocalPeer, seeds)

	// Register callbacks
	p2p.OnMakeMessage(spvWallet.makeMessage)
	p2p.OnHandleVersion(spvWallet.handleVersion)
	p2p.OnPeerConnected(spvWallet.peerConnected)
	p2p.OnHandleMessage(spvWallet.handleMessage)
	return spvWallet, nil
}

func toSPVAddr(seeds []string) []string {
	var addrs = make([]string, len(seeds))
	for i, seed := range seeds {
		portIndex := strings.LastIndex(seed, ":")
		if portIndex > 0 {
			addrs[i] = fmt.Sprint(string([]byte(seed)[:portIndex]), ":", sdk.SPVServerPort)
		} else {
			addrs[i] = fmt.Sprint(seed, ":", sdk.SPVServerPort)
		}
	}
	return addrs
}

type SPVWallet struct {
	*SyncManager
	clientId uint64
	chain    *Blockchain
	pm       *p2p.PeerManager
}

func (wallet *SPVWallet) initLocalPeer(peer *p2p.Peer) {
	peer.SetID(wallet.clientId)
	peer.SetVersion(p2p.ProtocolVersion)
	peer.SetPort(sdk.SPVClientPort)
	peer.SetServices(0x00)
	peer.SetRelay(0x00)
}

func (wallet *SPVWallet) makeMessage(cmd string) (message p2p.Message, err error) {
	switch cmd {
	case "ping":
		message = new(msg.Ping)
	case "pong":
		message = new(msg.Pong)
	case "inv":
		message = new(msg.Inventory)
	case "tx":
		message = new(msg.Txn)
	case "merkleblock":
		message = new(bloom.MerkleBlock)
	case "notfound":
		message = new(msg.NotFound)
	default:
		return nil, errors.New("Received unsupported message, CMD " + cmd)
	}
	return message, nil
}

func (wallet *SPVWallet) handleVersion(v *p2p.Version) error {

	if v.Version < p2p.ProtocolVersion {
		log.Error("SPV disconnect peer, To support SPV protocol, peer version must greater than ", p2p.ProtocolVersion)
		return errors.New(fmt.Sprint("To support SPV protocol, peer version must greater than ", p2p.ProtocolVersion))
	}

	if v.Services/sdk.ServiveSPV&1 == 0 {
		log.Error("SPV disconnect peer, spv service not enabled on connected peer")
		return errors.New("SPV service not enabled on connected peer")
	}

	return nil
}

func (wallet *SPVWallet) peerConnected(peer *p2p.Peer) {
	// Send filterload message
	peer.Send(wallet.chain.GetBloomFilter().GetFilterLoadMsg())
}

func (wallet *SPVWallet) handleMessage(peer *p2p.Peer, message p2p.Message) error {
	switch msg := message.(type) {
	case *msg.Ping:
		return wallet.OnPing(peer, msg)
	case *msg.Pong:
		return wallet.OnPong(peer, msg)
	case *msg.Inventory:
		return wallet.OnInventory(peer, msg)
	case *bloom.MerkleBlock:
		return wallet.OnMerkleBlock(peer, msg)
	case *msg.Txn:
		return wallet.OnTxn(peer, msg)
	case *msg.NotFound:
		return wallet.OnNotFound(peer, msg)
	default:
		return errors.New("unknown handle message type")
	}
}

func (wallet *SPVWallet) Start() {
	wallet.pm.Start()
	go wallet.keepUpdate()
	log.Info("SPV service started...")
}

func (wallet *SPVWallet) Stop() {
	wallet.chain.Close()
	log.Info("SPV service stopped...")
}

func (wallet *SPVWallet) BlockChain() *Blockchain {
	return wallet.chain
}

func (wallet *SPVWallet) keepUpdate() {
	ticker := time.NewTicker(time.Second * p2p.InfoUpdateDuration)
	defer ticker.Stop()
	for range ticker.C {

		// Update peers info
		for _, peer := range wallet.pm.ConnectedPeers() {
			if peer.State() == p2p.ESTABLISH {

				// Disconnect inactive peer
				if peer.LastActive().Before(
					time.Now().Add(-time.Second * p2p.InfoUpdateDuration * p2p.KeepAliveTimeout)) {
					log.Trace("SPV disconnect inactive peer,", peer)
					wallet.pm.DisconnectPeer(peer)
					continue
				}

				// Send ping message to peer
				go peer.Send(msg.NewPing(wallet.chain.Height()))
			}
		}

		// Keep connections
		wallet.pm.ConnectPeers()

		// Keep synchronizing blocks
		wallet.SyncBlocks()
	}
}

func (wallet *SPVWallet) OnPing(peer *p2p.Peer, p *msg.Ping) error {
	peer.SetHeight(p.Height)
	go peer.Send(msg.NewPong(wallet.chain.Height()))
	return nil
}

func (wallet *SPVWallet) OnPong(peer *p2p.Peer, p *msg.Pong) error {
	peer.SetHeight(p.Height)
	return nil
}

func (wallet *SPVWallet) OnInventory(peer *p2p.Peer, inv *msg.Inventory) error {
	switch inv.Type {
	case msg.TRANSACTION:
		// Do nothing, transaction inventory is not supported
	case msg.BLOCK:
		log.Info("SPV receive block inventory")
		return wallet.HandleBlockInvMsg(peer, inv)
	}
	return nil
}

func (wallet *SPVWallet) NotifyNewAddress(hash []byte) error {
	// Reload address filter to include new address
	wallet.chain.Addrs().ReloadAddrFilter()
	// Broadcast filterload message to connected peers
	wallet.pm.Broadcast(wallet.chain.GetBloomFilter().GetFilterLoadMsg())
	return nil
}

func (wallet *SPVWallet) SendTransaction(tx tx.Transaction) error {
	// Broadcast transaction to connected peers
	wallet.pm.Broadcast(msg.NewTxn(tx))
	return nil
}

func (wallet *SPVWallet) OnMerkleBlock(peer *p2p.Peer, block *bloom.MerkleBlock) error {
	wallet.dataLock.Lock()
	defer wallet.dataLock.Unlock()

	blockHash := block.BlockHeader.Hash()
	log.Trace("Receive merkle block hash:", blockHash.String())

	if wallet.chain.IsKnownBlock(*blockHash) {
		return errors.New(fmt.Sprint("Received block that already known,", blockHash.String()))
	}

	err := wallet.chain.CheckProofOfWork(&block.BlockHeader)
	if err != nil {
		return err
	}

	if wallet.chain.IsSyncing() && !wallet.InRequestQueue(*blockHash) {
		// Put non syncing blocks into orphan pool
		wallet.AddOrphanBlock(*blockHash, block)
		return nil
	}

	if !wallet.chain.IsSyncing() {
		// Check if new block can connect to previous
		tip := wallet.chain.ChainTip()
		// If block is already added, return
		if tip.Hash().IsEqual(blockHash) {
			return nil
		}
		// Meet an orphan block
		if !tip.Hash().IsEqual(&block.BlockHeader.Previous) {
			// Put non syncing blocks into orphan pool
			wallet.AddOrphanBlock(*blockHash, block)
			return nil
		}
		// Set start hash and stop hash to the same block hash
		wallet.startHash = blockHash
		wallet.stopHash = blockHash

	} else if wallet.blockLocator == nil || wallet.pm.GetSyncPeer() == nil || wallet.pm.GetSyncPeer().ID() != peer.ID() {

		log.Error("Receive message from non sync peer, disconnect")
		wallet.ChangeSyncPeerAndRestart()
		return errors.New("Receive message from non sync peer, disconnect")
	}
	// Mark block as received
	wallet.BlockReceived(*blockHash, block)

	return wallet.RequestBlockTxns(peer, block)
}

func (wallet *SPVWallet) OnTxn(peer *p2p.Peer, txn *msg.Txn) error {
	wallet.dataLock.Lock()
	defer wallet.dataLock.Unlock()

	txId := txn.Transaction.Hash()
	log.Debug("Receive transaction hash: ", txId.String())

	if wallet.chain.IsSyncing() && !wallet.InRequestQueue(*txId) {
		// Put non syncing txns into orphan pool
		wallet.AddOrphanTxn(*txId, txn)
		return nil
	}

	if !wallet.chain.IsSyncing() {
		// Check if transaction already received
		if wallet.MemCache.TxCached(*txId) {
			return errors.New("Received transaction already cached")
		}
		// Put txn into unconfirmed txnpool
		fPositive, err := wallet.chain.CommitUnconfirmedTxn(txn.Transaction)
		if err != nil {
			return err
		}
		if fPositive {
			wallet.handleFPositive(1)
		}

	} else if wallet.blockLocator == nil || wallet.pm.GetSyncPeer() == nil || wallet.pm.GetSyncPeer().ID() != peer.ID() {

		log.Error("Receive message from non sync peer, disconnect")
		wallet.ChangeSyncPeerAndRestart()
		return errors.New("Receive message from non sync peer, disconnect")
	}

	wallet.TxnReceived(*txId, txn)

	// All request finished, submit received block and txn data
	if wallet.RequestFinished() {

		err := wallet.CommitData()
		if err != nil {
			return err
		}

		// Continue syncing
		wallet.startSync()

		return nil
	}

	return nil
}

func (wallet *SPVWallet) OnNotFound(peer *p2p.Peer, msg *msg.NotFound) error {
	log.Error("Receive not found message, disconnect")
	wallet.ChangeSyncPeerAndRestart()
	return nil
}

func (wallet *SPVWallet) updateLocalHeight() {
	wallet.pm.Local().SetHeight(uint64(wallet.chain.Height()))
}