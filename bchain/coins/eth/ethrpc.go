package eth

import (
	"blockbook/bchain"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/golang/glog"
	"github.com/juju/errors"
)

// EthereumNet type specifies the type of ethereum network
type EthereumNet uint32

const (
	// MainNet is production network
	MainNet EthereumNet = 1
	// TestNet is Ropsten test network
	TestNet EthereumNet = 3
)

// Configuration represents json config file
type Configuration struct {
	CoinName                    string `json:"coin_name"`
	CoinShortcut                string `json:"coin_shortcut"`
	RPCURL                      string `json:"rpc_url"`
	RPCTimeout                  int    `json:"rpc_timeout"`
	BlockAddressesToKeep        int    `json:"block_addresses_to_keep"`
	MempoolTxTimeoutHours       int    `json:"mempoolTxTimeoutHours"`
	QueryBackendOnMempoolResync bool   `json:"queryBackendOnMempoolResync"`
}

// EthereumRPC is an interface to JSON-RPC eth service.
type EthereumRPC struct {
	*bchain.BaseChain
	client               *ethclient.Client
	rpc                  *rpc.Client
	timeout              time.Duration
	Parser               *EthereumParser
	Mempool              *bchain.MempoolEthereumType
	mempoolInitialized   bool
	bestHeaderLock       sync.Mutex
	bestHeader           *ethtypes.Header
	bestHeaderTime       time.Time
	chanNewBlock         chan *ethtypes.Header
	newBlockSubscription *rpc.ClientSubscription
	chanNewTx            chan ethcommon.Hash
	newTxSubscription    *rpc.ClientSubscription
	ChainConfig          *Configuration
	isETC                bool
}

// NewEthereumRPC returns new EthRPC instance.
func NewEthereumRPC(config json.RawMessage, pushHandler func(bchain.NotificationType)) (bchain.BlockChain, error) {
	var err error
	var c Configuration
	err = json.Unmarshal(config, &c)
	if err != nil {
		return nil, errors.Annotatef(err, "Invalid configuration file")
	}
	// keep at least 100 mappings block->addresses to allow rollback
	if c.BlockAddressesToKeep < 100 {
		c.BlockAddressesToKeep = 100
	}
	rc, err := rpc.Dial(c.RPCURL)
	if err != nil {
		return nil, err
	}
	ec := ethclient.NewClient(rc)

	s := &EthereumRPC{
		BaseChain:   &bchain.BaseChain{},
		client:      ec,
		rpc:         rc,
		ChainConfig: &c,
	}

	// always create parser
	s.Parser = NewEthereumParser(c.BlockAddressesToKeep)
	s.timeout = time.Duration(c.RPCTimeout) * time.Second

	// detect ethereum classic
	s.isETC = s.ChainConfig.CoinName == "Ethereum Classic"

	// new blocks notifications handling
	// the subscription is done in Initialize
	s.chanNewBlock = make(chan *ethtypes.Header)
	go func() {
		for {
			h, ok := <-s.chanNewBlock
			if !ok {
				break
			}
			glog.V(2).Info("rpc: new block header ", h.Number)
			// update best header to the new header
			s.bestHeaderLock.Lock()
			s.bestHeader = h
			s.bestHeaderTime = time.Now()
			s.bestHeaderLock.Unlock()
			// notify blockbook
			pushHandler(bchain.NotificationNewBlock)
		}
	}()

	// new mempool transaction notifications handling
	// the subscription is done in Initialize
	s.chanNewTx = make(chan ethcommon.Hash)
	go func() {
		for {
			t, ok := <-s.chanNewTx
			if !ok {
				break
			}
			hex := t.Hex()
			if glog.V(2) {
				glog.Info("rpc: new tx ", hex)
			}
			s.Mempool.AddTransactionToMempool(hex)
			pushHandler(bchain.NotificationNewTx)
		}
	}()

	return s, nil
}

// Initialize initializes ethereum rpc interface
func (b *EthereumRPC) Initialize() error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()

	id, err := b.client.NetworkID(ctx)
	if err != nil {
		return err
	}

	// parameters for getInfo request
	switch EthereumNet(id.Uint64()) {
	case MainNet:
		b.Testnet = false
		b.Network = "livenet"
		break
	case TestNet:
		b.Testnet = true
		b.Network = "testnet"
		break
	default:
		return errors.Errorf("Unknown network id %v", id)
	}
	glog.Info("rpc: block chain ", b.Network)

	return nil
}

// CreateMempool creates mempool if not already created, however does not initialize it
func (b *EthereumRPC) CreateMempool(chain bchain.BlockChain) (bchain.Mempool, error) {
	if b.Mempool == nil {
		b.Mempool = bchain.NewMempoolEthereumType(chain, b.ChainConfig.MempoolTxTimeoutHours, b.ChainConfig.QueryBackendOnMempoolResync)
		glog.Info("mempool created, MempoolTxTimeoutHours=", b.ChainConfig.MempoolTxTimeoutHours, ", QueryBackendOnMempoolResync=", b.ChainConfig.QueryBackendOnMempoolResync)
	}
	return b.Mempool, nil
}

// InitializeMempool creates subscriptions to newHeads and newPendingTransactions
func (b *EthereumRPC) InitializeMempool(addrDescForOutpoint bchain.AddrDescForOutpointFunc, onNewTxAddr bchain.OnNewTxAddrFunc) error {
	if b.Mempool == nil {
		return errors.New("Mempool not created")
	}

	// get initial mempool transactions
	txs, err := b.GetMempoolTransactions()
	if err != nil {
		return err
	}
	for _, txid := range txs {
		b.Mempool.AddTransactionToMempool(txid)
	}

	b.Mempool.OnNewTxAddr = onNewTxAddr

	if b.isETC {
		glog.Info(b.ChainConfig.CoinName, " does not support subscription to newHeads")
	} else {
		// subscriptions
		if err := b.subscribe(func() (*rpc.ClientSubscription, error) {
			// invalidate the previous subscription - it is either the first one or there was an error
			b.newBlockSubscription = nil
			ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
			defer cancel()
			sub, err := b.rpc.EthSubscribe(ctx, b.chanNewBlock, "newHeads")
			if err != nil {
				return nil, errors.Annotatef(err, "EthSubscribe newHeads")
			}
			b.newBlockSubscription = sub
			glog.Info("Subscribed to newHeads")
			return sub, nil
		}); err != nil {
			return err
		}
	}

	if err := b.subscribe(func() (*rpc.ClientSubscription, error) {
		// invalidate the previous subscription - it is either the first one or there was an error
		b.newTxSubscription = nil
		ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
		defer cancel()
		sub, err := b.rpc.EthSubscribe(ctx, b.chanNewTx, "newPendingTransactions")
		if err != nil {
			return nil, errors.Annotatef(err, "EthSubscribe newPendingTransactions")
		}
		b.newTxSubscription = sub
		glog.Info("Subscribed to newPendingTransactions")
		return sub, nil
	}); err != nil {
		return err
	}

	b.mempoolInitialized = true

	return nil
}

// subscribe subscribes notification and tries to resubscribe in case of error
func (b *EthereumRPC) subscribe(f func() (*rpc.ClientSubscription, error)) error {
	s, err := f()
	if err != nil {
		return err
	}
	go func() {
	Loop:
		for {
			// wait for error in subscription
			e := <-s.Err()
			// nil error means sub.Unsubscribe called, exit goroutine
			if e == nil {
				return
			}
			glog.Error("Subscription error ", e)
			timer := time.NewTimer(time.Second * 2)
			// try in 1 second interval to resubscribe
			for {
				select {
				case e = <-s.Err():
					if e == nil {
						return
					}
				case <-timer.C:
					ns, err := f()
					if err == nil {
						// subscription successful, restart wait for next error
						s = ns
						continue Loop
					}
					glog.Error("Resubscribe error ", e)
					timer.Reset(time.Second * 2)
				}
			}
		}
	}()
	return nil
}

// Shutdown cleans up rpc interface to ethereum
func (b *EthereumRPC) Shutdown(ctx context.Context) error {
	if b.newBlockSubscription != nil {
		b.newBlockSubscription.Unsubscribe()
	}
	if b.newTxSubscription != nil {
		b.newTxSubscription.Unsubscribe()
	}
	if b.rpc != nil {
		b.rpc.Close()
	}
	close(b.chanNewBlock)
	glog.Info("rpc: shutdown")
	return nil
}

// GetCoinName returns coin name
func (b *EthereumRPC) GetCoinName() string {
	return b.ChainConfig.CoinName
}

// GetSubversion returns empty string, ethereum does not have subversion
func (b *EthereumRPC) GetSubversion() string {
	return ""
}

// GetChainInfo returns information about the connected backend
func (b *EthereumRPC) GetChainInfo() (*bchain.ChainInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	id, err := b.client.NetworkID(ctx)
	if err != nil {
		return nil, err
	}
	h, err := b.getBestHeader()
	if err != nil {
		return nil, err
	}
	var ver, protocol string
	if err := b.rpc.CallContext(ctx, &ver, "web3_clientVersion"); err != nil {
		return nil, err
	}
	if err := b.rpc.CallContext(ctx, &protocol, "eth_protocolVersion"); err != nil {
		return nil, err
	}
	rv := &bchain.ChainInfo{
		Blocks:          int(h.Number.Int64()),
		Bestblockhash:   h.Hash().Hex(),
		Difficulty:      h.Difficulty.String(),
		Version:         ver,
		ProtocolVersion: protocol,
	}
	idi := int(id.Uint64())
	if idi == 1 {
		rv.Chain = "mainnet"
	} else {
		rv.Chain = "testnet " + strconv.Itoa(idi)
	}
	return rv, nil
}

func (b *EthereumRPC) getBestHeader() (*ethtypes.Header, error) {
	b.bestHeaderLock.Lock()
	defer b.bestHeaderLock.Unlock()
	// ETC does not have newBlocks subscription, bestHeader must be updated very often (each 1 second)
	if b.isETC {
		if b.bestHeaderTime.Add(1 * time.Second).Before(time.Now()) {
			b.bestHeader = nil
		}
	}
	if b.bestHeader == nil {
		var err error
		ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
		defer cancel()
		b.bestHeader, err = b.client.HeaderByNumber(ctx, nil)
		if err != nil {
			return nil, err
		}
		b.bestHeaderTime = time.Now()
	}
	return b.bestHeader, nil
}

// GetBestBlockHash returns hash of the tip of the best-block-chain
func (b *EthereumRPC) GetBestBlockHash() (string, error) {
	h, err := b.getBestHeader()
	if err != nil {
		return "", err
	}
	return h.Hash().Hex(), nil
}

// GetBestBlockHeight returns height of the tip of the best-block-chain
func (b *EthereumRPC) GetBestBlockHeight() (uint32, error) {
	h, err := b.getBestHeader()
	if err != nil {
		return 0, err
	}
	return uint32(h.Number.Uint64()), nil
}

// GetBlockHash returns hash of block in best-block-chain at given height
func (b *EthereumRPC) GetBlockHash(height uint32) (string, error) {
	var n big.Int
	n.SetUint64(uint64(height))
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	h, err := b.client.HeaderByNumber(ctx, &n)
	if err != nil {
		if err == ethereum.NotFound {
			return "", bchain.ErrBlockNotFound
		}
		return "", errors.Annotatef(err, "height %v", height)
	}
	return h.Hash().Hex(), nil
}

func (b *EthereumRPC) ethHeaderToBlockHeader(h *rpcHeader) (*bchain.BlockHeader, error) {
	height, err := ethNumber(h.Number)
	if err != nil {
		return nil, err
	}
	c, err := b.computeConfirmations(uint64(height))
	if err != nil {
		return nil, err
	}
	time, err := ethNumber(h.Time)
	if err != nil {
		return nil, err
	}
	size, err := ethNumber(h.Size)
	if err != nil {
		return nil, err
	}
	return &bchain.BlockHeader{
		Hash:          h.Hash,
		Prev:          h.ParentHash,
		Height:        uint32(height),
		Confirmations: int(c),
		Time:          time,
		Size:          int(size),
	}, nil
}

// GetBlockHeader returns header of block with given hash
func (b *EthereumRPC) GetBlockHeader(hash string) (*bchain.BlockHeader, error) {
	raw, err := b.getBlockRaw(hash, 0, false)
	if err != nil {
		return nil, err
	}
	var h rpcHeader
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, errors.Annotatef(err, "hash %v", hash)
	}
	return b.ethHeaderToBlockHeader(&h)
}

func (b *EthereumRPC) computeConfirmations(n uint64) (uint32, error) {
	bh, err := b.getBestHeader()
	if err != nil {
		return 0, err
	}
	bn := bh.Number.Uint64()
	// transaction in the best block has 1 confirmation
	return uint32(bn - n + 1), nil
}

func (b *EthereumRPC) getBlockRaw(hash string, height uint32, fullTxs bool) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	var raw json.RawMessage
	var err error
	if hash != "" {
		if hash == "pending" {
			err = b.rpc.CallContext(ctx, &raw, "eth_getBlockByNumber", hash, fullTxs)
		} else {
			err = b.rpc.CallContext(ctx, &raw, "eth_getBlockByHash", ethcommon.HexToHash(hash), fullTxs)
		}
	} else {
		err = b.rpc.CallContext(ctx, &raw, "eth_getBlockByNumber", fmt.Sprintf("%#x", height), fullTxs)
	}
	if err != nil {
		return nil, errors.Annotatef(err, "hash %v, height %v", hash, height)
	} else if len(raw) == 0 {
		return nil, bchain.ErrBlockNotFound
	}
	return raw, nil
}

func (b *EthereumRPC) getERC20EventsForBlock(blockNumber string) (map[string][]*rpcLog, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	var logs []rpcLogWithTxHash
	err := b.rpc.CallContext(ctx, &logs, "eth_getLogs", map[string]interface{}{
		"fromBlock": blockNumber,
		"toBlock":   blockNumber,
		"topics":    []string{erc20TransferEventSignature},
	})
	if err != nil {
		return nil, errors.Annotatef(err, "blockNumber %v", blockNumber)
	}
	r := make(map[string][]*rpcLog)
	for i := range logs {
		l := &logs[i]
		r[l.Hash] = append(r[l.Hash], &l.rpcLog)
	}
	return r, nil
}

// GetBlock returns block with given hash or height, hash has precedence if both passed
func (b *EthereumRPC) GetBlock(hash string, height uint32) (*bchain.Block, error) {
	raw, err := b.getBlockRaw(hash, height, true)
	if err != nil {
		return nil, err
	}
	var head rpcHeader
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, errors.Annotatef(err, "hash %v, height %v", hash, height)
	}
	var body rpcBlockTransactions
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.Annotatef(err, "hash %v, height %v", hash, height)
	}
	bbh, err := b.ethHeaderToBlockHeader(&head)
	if err != nil {
		return nil, errors.Annotatef(err, "hash %v, height %v", hash, height)
	}
	// get ERC20 events
	logs, err := b.getERC20EventsForBlock(head.Number)
	if err != nil {
		return nil, err
	}
	btxs := make([]bchain.Tx, len(body.Transactions))
	for i := range body.Transactions {
		tx := &body.Transactions[i]
		btx, err := b.Parser.ethTxToTx(tx, &rpcReceipt{Logs: logs[tx.Hash]}, bbh.Time, uint32(bbh.Confirmations))
		if err != nil {
			return nil, errors.Annotatef(err, "hash %v, height %v, txid %v", hash, height, tx.Hash)
		}
		btxs[i] = *btx
		if b.mempoolInitialized {
			b.Mempool.RemoveTransactionFromMempool(tx.Hash)
		}
	}
	bbk := bchain.Block{
		BlockHeader: *bbh,
		Txs:         btxs,
	}
	return &bbk, nil
}

// GetBlockInfo returns extended header (more info than in bchain.BlockHeader) with a list of txids
func (b *EthereumRPC) GetBlockInfo(hash string) (*bchain.BlockInfo, error) {
	raw, err := b.getBlockRaw(hash, 0, false)
	if err != nil {
		return nil, err
	}
	var head rpcHeader
	var txs rpcBlockTxids
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, errors.Annotatef(err, "hash %v", hash)
	}
	if err = json.Unmarshal(raw, &txs); err != nil {
		return nil, err
	}
	bch, err := b.ethHeaderToBlockHeader(&head)
	if err != nil {
		return nil, err
	}
	return &bchain.BlockInfo{
		BlockHeader: *bch,
		Difficulty:  json.Number(head.Difficulty),
		Nonce:       json.Number(head.Nonce),
		Txids:       txs.Transactions,
	}, nil
}

// GetTransactionForMempool returns a transaction by the transaction ID.
// It could be optimized for mempool, i.e. without block time and confirmations
func (b *EthereumRPC) GetTransactionForMempool(txid string) (*bchain.Tx, error) {
	return b.GetTransaction(txid)
}

// GetTransaction returns a transaction by the transaction ID.
func (b *EthereumRPC) GetTransaction(txid string) (*bchain.Tx, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	var tx *rpcTransaction
	hash := ethcommon.HexToHash(txid)
	err := b.rpc.CallContext(ctx, &tx, "eth_getTransactionByHash", hash)
	if err != nil {
		return nil, err
	} else if tx == nil {
		if b.mempoolInitialized {
			b.Mempool.RemoveTransactionFromMempool(txid)
		}
		return nil, bchain.ErrTxNotFound
	}
	var btx *bchain.Tx
	if tx.BlockNumber == "" {
		// mempool tx
		btx, err = b.Parser.ethTxToTx(tx, nil, 0, 0)
		if err != nil {
			return nil, errors.Annotatef(err, "txid %v", txid)
		}
	} else {
		// non mempool tx - read the block header to get the block time
		raw, err := b.getBlockRaw(tx.BlockHash, 0, false)
		if err != nil {
			return nil, err
		}
		var ht struct {
			Time string `json:"timestamp"`
		}
		if err := json.Unmarshal(raw, &ht); err != nil {
			return nil, errors.Annotatef(err, "hash %v", hash)
		}
		var time int64
		if time, err = ethNumber(ht.Time); err != nil {
			return nil, errors.Annotatef(err, "txid %v", txid)
		}
		var receipt rpcReceipt
		if b.isETC {
			var rawReceipt json.RawMessage
			var etcReceipt rpcEtcReceipt
			err = b.rpc.CallContext(ctx, &rawReceipt, "eth_getTransactionReceipt", hash)
			if err != nil {
				return nil, errors.Annotatef(err, "txid %v", txid)
			}
			err = json.Unmarshal(rawReceipt, &etcReceipt)
			if err == nil {
				receipt.GasUsed = etcReceipt.GasUsed
				receipt.Logs = etcReceipt.Logs
				if etcReceipt.Status == 0 {
					receipt.Status = "0x0"
				} else {
					receipt.Status = "0x1"
				}
			} else {
				err = json.Unmarshal(rawReceipt, &receipt)
				if err != nil {
					return nil, errors.Annotatef(err, "unmarshal receipt for txid %v, %v", txid, string(rawReceipt))
				}
			}
		} else {
			err = b.rpc.CallContext(ctx, &receipt, "eth_getTransactionReceipt", hash)
			if err != nil {
				return nil, errors.Annotatef(err, "txid %v", txid)
			}
		}
		n, err := ethNumber(tx.BlockNumber)
		if err != nil {
			return nil, errors.Annotatef(err, "txid %v", txid)
		}
		confirmations, err := b.computeConfirmations(uint64(n))
		if err != nil {
			return nil, errors.Annotatef(err, "txid %v", txid)
		}
		btx, err = b.Parser.ethTxToTx(tx, &receipt, time, confirmations)
		if err != nil {
			return nil, errors.Annotatef(err, "txid %v", txid)
		}
		// remove tx from mempool if it is there
		if b.mempoolInitialized {
			b.Mempool.RemoveTransactionFromMempool(txid)
		}
	}
	return btx, nil
}

// GetTransactionSpecific returns json as returned by backend, with all coin specific data
func (b *EthereumRPC) GetTransactionSpecific(tx *bchain.Tx) (json.RawMessage, error) {
	csd, ok := tx.CoinSpecificData.(completeTransaction)
	if !ok {
		ntx, err := b.GetTransaction(tx.Txid)
		if err != nil {
			return nil, err
		}
		csd, ok = ntx.CoinSpecificData.(completeTransaction)
		if !ok {
			return nil, errors.New("Cannot get CoinSpecificData")
		}
	}
	m, err := json.Marshal(&csd)
	return json.RawMessage(m), err
}

// GetMempoolTransactions returns transactions in mempool
func (b *EthereumRPC) GetMempoolTransactions() ([]string, error) {
	raw, err := b.getBlockRaw("pending", 0, false)
	if err != nil {
		return nil, err
	}
	var body rpcBlockTxids
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
	}
	return body.Transactions, nil
}

// EstimateFee returns fee estimation
func (b *EthereumRPC) EstimateFee(blocks int) (big.Int, error) {
	return b.EstimateSmartFee(blocks, true)
}

// EstimateSmartFee returns fee estimation
func (b *EthereumRPC) EstimateSmartFee(blocks int, conservative bool) (big.Int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	var r big.Int
	gp, err := b.client.SuggestGasPrice(ctx)
	if err == nil && b != nil {
		r = *gp
	}
	return r, err
}

func getStringFromMap(p string, params map[string]interface{}) (string, bool) {
	v, ok := params[p]
	if ok {
		s, ok := v.(string)
		return s, ok
	}
	return "", false
}

// EthereumTypeEstimateGas returns estimation of gas consumption for given transaction parameters
func (b *EthereumRPC) EthereumTypeEstimateGas(params map[string]interface{}) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	msg := ethereum.CallMsg{}
	s, ok := getStringFromMap("from", params)
	if ok && len(s) > 0 {
		msg.From = ethcommon.HexToAddress(s)
	}
	s, ok = getStringFromMap("to", params)
	if ok && len(s) > 0 {
		a := ethcommon.HexToAddress(s)
		msg.To = &a
	}
	s, ok = getStringFromMap("data", params)
	if ok && len(s) > 0 {
		msg.Data = ethcommon.FromHex(s)
	}
	return b.client.EstimateGas(ctx, msg)
}

// SendRawTransaction sends raw transaction
func (b *EthereumRPC) SendRawTransaction(hex string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	var raw json.RawMessage
	err := b.rpc.CallContext(ctx, &raw, "eth_sendRawTransaction", hex)
	if err != nil {
		return "", err
	} else if len(raw) == 0 {
		return "", errors.New("SendRawTransaction: failed")
	}
	var result string
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", errors.Annotatef(err, "raw result %v", raw)
	}
	if result == "" {
		return "", errors.New("SendRawTransaction: failed, empty result")
	}
	return result, nil
}

// EthereumTypeGetBalance returns current balance of an address
func (b *EthereumRPC) EthereumTypeGetBalance(addrDesc bchain.AddressDescriptor) (*big.Int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	return b.client.BalanceAt(ctx, ethcommon.BytesToAddress(addrDesc), nil)
}

// EthereumTypeGetNonce returns current balance of an address
func (b *EthereumRPC) EthereumTypeGetNonce(addrDesc bchain.AddressDescriptor) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	return b.client.NonceAt(ctx, ethcommon.BytesToAddress(addrDesc), nil)
}

// GetChainParser returns ethereum BlockChainParser
func (b *EthereumRPC) GetChainParser() bchain.BlockChainParser {
	return b.Parser
}
