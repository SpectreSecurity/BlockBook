package db

import (
	"blockbook/bchain"
	"blockbook/bchain/coins/eth"
	"blockbook/common"

	"github.com/golang/glog"
	"github.com/juju/errors"
)

// TxCache is handle to TxCacheServer
type TxCache struct {
	db        *RocksDB
	chain     bchain.BlockChain
	metrics   *common.Metrics
	is        *common.InternalState
	enabled   bool
	chainType bchain.ChainType
}

// NewTxCache creates new TxCache interface and returns its handle
func NewTxCache(db *RocksDB, chain bchain.BlockChain, metrics *common.Metrics, is *common.InternalState, enabled bool) (*TxCache, error) {
	if !enabled {
		glog.Info("txcache: disabled")
	}
	return &TxCache{
		db:        db,
		chain:     chain,
		metrics:   metrics,
		is:        is,
		enabled:   enabled,
		chainType: chain.GetChainParser().GetChainType(),
	}, nil
}

// GetTransaction returns transaction either from RocksDB or if not present from blockchain
// it the transaction is confirmed, it is stored in the RocksDB
func (c *TxCache) GetTransaction(txid string) (*bchain.Tx, uint32, error) {
	var tx *bchain.Tx
	var h uint32
	var err error
	if c.enabled {
		tx, h, err = c.db.GetTx(txid)
		if err != nil {
			return nil, 0, err
		}
		if tx != nil {
			// number of confirmations is not stored in cache, they change all the time
			_, bestheight, _ := c.is.GetSyncState()
			tx.Confirmations = bestheight - h + 1
			c.metrics.TxCacheEfficiency.With(common.Labels{"status": "hit"}).Inc()
			return tx, h, nil
		}
	}
	tx, err = c.chain.GetTransaction(txid)
	if err != nil {
		return nil, 0, err
	}
	c.metrics.TxCacheEfficiency.With(common.Labels{"status": "miss"}).Inc()
	// cache only confirmed transactions
	if tx.Confirmations > 0 {
		if c.chainType == bchain.ChainBitcoinType {
			ta, err := c.db.GetTxAddresses(txid)
			if err != nil {
				return nil, 0, err
			}
			// the transaction may me not yet indexed, in that case get the height from the backend
			if ta == nil {
				h, err = c.chain.GetBestBlockHeight()
				if err != nil {
					return nil, 0, err
				}
			} else {
				h = ta.Height
			}
		} else if c.chainType == bchain.ChainEthereumType {
			h, err = eth.GetHeightFromTx(tx)
			if err != nil {
				return nil, 0, err
			}
		} else {
			return nil, 0, errors.New("Unknown chain type")
		}
		if c.enabled {
			err = c.db.PutTx(tx, h, tx.Blocktime)
			// do not return caching error, only log it
			if err != nil {
				glog.Error("PutTx error ", err)
			}
		}
	} else {
		h = 0
	}
	return tx, h, nil
}
