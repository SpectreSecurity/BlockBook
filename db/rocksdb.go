package db

import (
    "blockbook/bchain"
    "blockbook/common"
    "bytes"
    "encoding/binary"
    "encoding/hex"
    "fmt"
    "math/big"
    "os"
    "path/filepath"
    "sort"
    "strconv"
    "time"
    "unsafe"

    vlq "github.com/bsm/go-vlq"
    "github.com/golang/glog"
    "github.com/juju/errors"
    "github.com/tecbot/gorocksdb"
    "github.com/martinboehm/btcutil/txscript"
)

const dbVersion = 5

const packedHeightBytes = 4
const maxAddrDescLen = 1024

// iterator creates snapshot, which takes lots of resources
// when doing huge scan, it is better to close it and reopen from time to time to free the resources
const refreshIterator = 5000000

// RepairRocksDB calls RocksDb db repair function
func RepairRocksDB(name string) error {
    glog.Infof("rocksdb: repair")
    opts := gorocksdb.NewDefaultOptions()
    return gorocksdb.RepairDb(name, opts)
}

type connectBlockStats struct {
    txAddressesHit  int
    txAddressesMiss int
    balancesHit     int
    balancesMiss    int
}

// AddressBalanceDetail specifies what data are returned by GetAddressBalance
type AddressBalanceDetail int

const (
    // AddressBalanceDetailNoUTXO returns address balance without utxos
    AddressBalanceDetailNoUTXO = 0
    // AddressBalanceDetailUTXO returns address balance with utxos
    AddressBalanceDetailUTXO = 1
    // addressBalanceDetailUTXOIndexed returns address balance with utxos and index for updates, used only internally
    addressBalanceDetailUTXOIndexed = 2
)

// RocksDB handle
type RocksDB struct {
    path         string
    db           *gorocksdb.DB
    wo           *gorocksdb.WriteOptions
    ro           *gorocksdb.ReadOptions
    cfh          []*gorocksdb.ColumnFamilyHandle
    chainParser  bchain.BlockChainParser
    is           *common.InternalState
    metrics      *common.Metrics
    cache        *gorocksdb.Cache
    maxOpenFiles int
    cbs          connectBlockStats
}

const (
    cfDefault = iota
    cfHeight
    cfAddresses
    cfBlockTxs
    cfTransactions
    // BitcoinType
    cfAddressBalance
    cfTxAddresses
    // EthereumType
    cfAddressContracts = cfAddressBalance
)

// common columns
var cfNames []string
var cfBaseNames = []string{"default", "height", "addresses", "blockTxs", "transactions"}

// type specific columns
var cfNamesBitcoinType = []string{"addressBalance", "txAddresses"}
var cfNamesEthereumType = []string{"addressContracts"}

func openDB(path string, c *gorocksdb.Cache, openFiles int) (*gorocksdb.DB, []*gorocksdb.ColumnFamilyHandle, error) {
    // opts with bloom filter
    opts := createAndSetDBOptions(10, c, openFiles)
    // opts for addresses without bloom filter
    // from documentation: if most of your queries are executed using iterators, you shouldn't set bloom filter
    optsAddresses := createAndSetDBOptions(0, c, openFiles)
    // default, height, addresses, blockTxids, transactions
    cfOptions := []*gorocksdb.Options{opts, opts, optsAddresses, opts, opts}
    // append type specific options
    count := len(cfNames) - len(cfOptions)
    for i := 0; i < count; i++ {
        cfOptions = append(cfOptions, opts)
    }
    db, cfh, err := gorocksdb.OpenDbColumnFamilies(opts, path, cfNames, cfOptions)
    if err != nil {
        return nil, nil, err
    }
    return db, cfh, nil
}

// NewRocksDB opens an internal handle to RocksDB environment.  Close
// needs to be called to release it.
func NewRocksDB(path string, cacheSize, maxOpenFiles int, parser bchain.BlockChainParser, metrics *common.Metrics) (d *RocksDB, err error) {
    glog.Infof("rocksdb: opening %s, required data version %v, cache size %v, max open files %v", path, dbVersion, cacheSize, maxOpenFiles)

    cfNames = append([]string{}, cfBaseNames...)
    chainType := parser.GetChainType()
    if chainType == bchain.ChainBitcoinType {
        cfNames = append(cfNames, cfNamesBitcoinType...)
    } else if chainType == bchain.ChainEthereumType {
        cfNames = append(cfNames, cfNamesEthereumType...)
    } else {
        return nil, errors.New("Unknown chain type")
    }

    c := gorocksdb.NewLRUCache(cacheSize)
    db, cfh, err := openDB(path, c, maxOpenFiles)
    if err != nil {
        return nil, err
    }
    wo := gorocksdb.NewDefaultWriteOptions()
    ro := gorocksdb.NewDefaultReadOptions()
    return &RocksDB{path, db, wo, ro, cfh, parser, nil, metrics, c, maxOpenFiles, connectBlockStats{}}, nil
}

func (d *RocksDB) closeDB() error {
    for _, h := range d.cfh {
        h.Destroy()
    }
    d.db.Close()
    d.db = nil
    return nil
}

// Close releases the RocksDB environment opened in NewRocksDB.
func (d *RocksDB) Close() error {
    if d.db != nil {
        // store the internal state of the app
        if d.is != nil && d.is.DbState == common.DbStateOpen {
            d.is.DbState = common.DbStateClosed
            if err := d.StoreInternalState(d.is); err != nil {
                glog.Info("internalState: ", err)
            }
        }
        glog.Infof("rocksdb: close")
        d.closeDB()
        d.wo.Destroy()
        d.ro.Destroy()
    }
    return nil
}

// Reopen reopens the database
// It closes and reopens db, nobody can access the database during the operation!
func (d *RocksDB) Reopen() error {
    err := d.closeDB()
    if err != nil {
        return err
    }
    d.db = nil
    db, cfh, err := openDB(d.path, d.cache, d.maxOpenFiles)
    if err != nil {
        return err
    }
    d.db, d.cfh = db, cfh
    return nil
}

func atoi(s string) int {
    i, err := strconv.Atoi(s)
    if err != nil {
        return 0
    }
    return i
}

// GetMemoryStats returns memory usage statistics as reported by RocksDB
func (d *RocksDB) GetMemoryStats() string {
    var total, indexAndFilter, memtable int
    type columnStats struct {
        name           string
        indexAndFilter string
        memtable       string
    }
    cs := make([]columnStats, len(cfNames))
    for i := 0; i < len(cfNames); i++ {
        cs[i].name = cfNames[i]
        cs[i].indexAndFilter = d.db.GetPropertyCF("rocksdb.estimate-table-readers-mem", d.cfh[i])
        cs[i].memtable = d.db.GetPropertyCF("rocksdb.cur-size-all-mem-tables", d.cfh[i])
        indexAndFilter += atoi(cs[i].indexAndFilter)
        memtable += atoi(cs[i].memtable)
    }
    m := struct {
        cacheUsage       int
        pinnedCacheUsage int
        columns          []columnStats
    }{
        cacheUsage:       d.cache.GetUsage(),
        pinnedCacheUsage: d.cache.GetPinnedUsage(),
        columns:          cs,
    }
    total = m.cacheUsage + indexAndFilter + memtable
    return fmt.Sprintf("Total %d, indexAndFilter %d, memtable %d, %+v", total, indexAndFilter, memtable, m)
}

// StopIteration is returned by callback function to signal stop of iteration
type StopIteration struct{}

func (e *StopIteration) Error() string {
    return ""
}

// GetTransactionsCallback is called by GetTransactions/GetAddrDescTransactions for each found tx
// indexes contain array of indexes (input negative, output positive) in tx where is given address
type GetTransactionsCallback func(txid string, height uint32, indexes []int32) error

// GetTransactions finds all input/output transactions for address
// Transaction are passed to callback function.
func (d *RocksDB) GetTransactions(address string, lower uint32, higher uint32, fn GetTransactionsCallback) (err error) {
    if glog.V(1) {
        glog.Infof("rocksdb: address get %s %d-%d ", address, lower, higher)
    }
    addrDesc, err := d.chainParser.GetAddrDescFromAddress(address)
    if err != nil {
        return err
    }
    return d.GetAddrDescTransactions(addrDesc, lower, higher, fn)
}

// GetAddrDescTransactions finds all input/output transactions for address descriptor
// Transaction are passed to callback function in the order from newest block to the oldest
func (d *RocksDB) GetAddrDescTransactions(addrDesc bchain.AddressDescriptor, lower uint32, higher uint32, fn GetTransactionsCallback) (err error) {
    txidUnpackedLen := d.chainParser.PackedTxidLen()
    startKey := packAddressKey(addrDesc, higher)
    stopKey := packAddressKey(addrDesc, lower)
    indexes := make([]int32, 0, 16)
    it := d.db.NewIteratorCF(d.ro, d.cfh[cfAddresses])
    defer it.Close()
    for it.Seek(startKey); it.Valid(); it.Next() {
        key := it.Key().Data()
        val := it.Value().Data()
        if bytes.Compare(key, stopKey) > 0 {
            break
        }
        if glog.V(2) {
            glog.Infof("rocksdb: addresses %s: %s", hex.EncodeToString(key), hex.EncodeToString(val))
        }
        _, height, err := unpackAddressKey(key)
        if err != nil {
            return err
        }
        for len(val) > txidUnpackedLen {
            tx, err := d.chainParser.UnpackTxid(val[:txidUnpackedLen])
            if err != nil {
                return err
            }
            indexes = indexes[:0]
            val = val[txidUnpackedLen:]
            for {
                index, l := unpackVarint32(val)
                indexes = append(indexes, index>>1)
                val = val[l:]
                if index&1 == 1 {
                    break
                } else if len(val) == 0 {
                    glog.Warningf("rocksdb: addresses contain incorrect data %s: %s", hex.EncodeToString(key), hex.EncodeToString(val))
                    break
                }
            }
            if err := fn(tx, height, indexes); err != nil {
                if _, ok := err.(*StopIteration); ok {
                    return nil
                }
                return err
            }
        }
        if len(val) != 0 {
            glog.Warningf("rocksdb: addresses contain incorrect data %s: %s", hex.EncodeToString(key), hex.EncodeToString(val))
        }
    }
    return nil
}

const (
    opInsert = 0
    opDelete = 1
)

// ConnectBlock indexes addresses in the block and stores them in db
func (d *RocksDB) ConnectBlock(block *bchain.Block) error {
    wb := gorocksdb.NewWriteBatch()
    defer wb.Destroy()

    if glog.V(2) {
        glog.Infof("rocksdb: insert %d %s", block.Height, block.Hash)
    }

    chainType := d.chainParser.GetChainType()

    if err := d.writeHeightFromBlock(wb, block, opInsert); err != nil {
        return err
    }
    addresses := make(addressesMap)
    if chainType == bchain.ChainBitcoinType {
        txAddressesMap := make(map[string]*TxAddresses)
        balances := make(map[string]*AddrBalance)
        if err := d.processAddressesBitcoinType(block, addresses, txAddressesMap, balances); err != nil {
            return err
        }
        if err := d.storeTxAddresses(wb, txAddressesMap); err != nil {
            return err
        }
        if err := d.storeBalances(wb, balances); err != nil {
            return err
        }
        if err := d.storeAndCleanupBlockTxs(wb, block); err != nil {
            return err
        }
    } else if chainType == bchain.ChainEthereumType {
        addressContracts := make(map[string]*AddrContracts)
        blockTxs, err := d.processAddressesEthereumType(block, addresses, addressContracts)
        if err != nil {
            return err
        }
        if err := d.storeAddressContracts(wb, addressContracts); err != nil {
            return err
        }
        if err := d.storeAndCleanupBlockTxsEthereumType(wb, block, blockTxs); err != nil {
            return err
        }
    } else {
        return errors.New("Unknown chain type")
    }
    if err := d.storeAddresses(wb, block.Height, addresses); err != nil {
        return err
    }

    return d.db.Write(d.wo, wb)
}

// Addresses index

type txIndexes struct {
    btxID   []byte
    indexes []int32
}

// addressesMap is a map of addresses in a block
// each address contains a slice of transactions with indexes where the address appears
// slice is used instead of map so that order is defined and also search in case of few items
type addressesMap map[string][]txIndexes

type outpoint struct {
    btxID []byte
    index int32
}

// TxInput holds input data of the transaction in TxAddresses
type TxInput struct {
    AddrDesc bchain.AddressDescriptor
    ValueSat big.Int
}

// Addresses converts AddressDescriptor of the input to array of strings
func (ti *TxInput) Addresses(p bchain.BlockChainParser) ([]string, bool, error) {
    return p.GetAddressesFromAddrDesc(ti.AddrDesc)
}

// TxOutput holds output data of the transaction in TxAddresses
type TxOutput struct {
    AddrDesc bchain.AddressDescriptor
    Spent    bool
    ValueSat big.Int
}

// Addresses converts AddressDescriptor of the output to array of strings
func (to *TxOutput) Addresses(p bchain.BlockChainParser) ([]string, bool, error) {
    return p.GetAddressesFromAddrDesc(to.AddrDesc)
}

// TxAddresses stores transaction inputs and outputs with amounts
type TxAddresses struct {
    Height  uint32
    Inputs  []TxInput
    Outputs []TxOutput
    ShieldIns    uint32
    ShieldOuts   uint32
    ShieldValBal big.Int
}

// Utxo holds information about unspent transaction output
type Utxo struct {
    BtxID    []byte
    Vout     int32
    Height   uint32
    ValueSat big.Int
}

// AddrBalance stores number of transactions and balances of an address
type AddrBalance struct {
    Txs        uint32
    SentSat    big.Int
    BalanceSat big.Int
    Utxos      []Utxo
    utxosMap   map[string]int
}

// ReceivedSat computes received amount from total balance and sent amount
func (ab *AddrBalance) ReceivedSat() *big.Int {
    var r big.Int
    r.Add(&ab.BalanceSat, &ab.SentSat)
    return &r
}

// Check if addressBalance has an utxo
func (ab *AddrBalance) hasUtxo(btxID []byte, vout int32) bool {
    if len(ab.utxosMap) > 0 {
        if i, ok := ab.utxosMap[string(btxID)]; ok {
            if ab.Utxos[i].Vout == vout {
                return true
            }
        }
        return false;
    } else {
        for _, utxo := range ab.Utxos {
            if (string(utxo.BtxID) == string(btxID)) &&
                        (utxo.Vout == vout) {
                return true
            }
        }
        return false;
    }
}

// addUtxo
func (ab *AddrBalance) addUtxo(u *Utxo) {
    ab.Utxos = append(ab.Utxos, *u)
    l := len(ab.Utxos)
    if l >= 16 {
        if len(ab.utxosMap) == 0 {
            ab.utxosMap = make(map[string]int, 32)
            for i := 0; i < l; i++ {
                s := string(ab.Utxos[i].BtxID)
                if _, e := ab.utxosMap[s]; !e {
                    ab.utxosMap[s] = i
                }
            }
        } else {
            s := string(u.BtxID)
            if _, e := ab.utxosMap[s]; !e {
                ab.utxosMap[s] = l - 1
            }
        }
    }
}

// markUtxoAsSpent finds outpoint btxID:vout in utxos and marks it as spent
// for small number of utxos the linear search is done, for larger number there is a hashmap index
// it is much faster than removing the utxo from the slice as it would cause in memory copy operations
func (ab *AddrBalance) markUtxoAsSpent(btxID []byte, vout int32) {
    if len(ab.utxosMap) == 0 {
        for i := range ab.Utxos {
            utxo := &ab.Utxos[i]
            if utxo.Vout == vout && *(*int)(unsafe.Pointer(&utxo.BtxID[0])) == *(*int)(unsafe.Pointer(&btxID[0])) && bytes.Equal(utxo.BtxID, btxID) {
                // mark utxo as spent by setting vout=-1
                utxo.Vout = -1
                return
            }
        }
    } else {
        if i, e := ab.utxosMap[string(btxID)]; e {
            l := len(ab.Utxos)
            for ; i < l; i++ {
                utxo := &ab.Utxos[i]
                if utxo.Vout == vout {
                    if bytes.Equal(utxo.BtxID, btxID) {
                        // mark utxo as spent by setting vout=-1
                        utxo.Vout = -1
                        return
                    }
                    break
                }
            }
        }
    }
    glog.Errorf("Utxo %s:%d not found, using in map %v", hex.EncodeToString(btxID), vout, len(ab.utxosMap) != 0)
}

type blockTxs struct {
    btxID  []byte
    inputs []outpoint
}

func (d *RocksDB) resetValueSatToZero(valueSat *big.Int, addrDesc bchain.AddressDescriptor, logText string) {
    ad, _, err := d.chainParser.GetAddressesFromAddrDesc(addrDesc)
    if err != nil {
        glog.Warningf("rocksdb: unparsable address hex '%v' reached negative %s %v, resetting to 0. Parser error %v", addrDesc, logText, valueSat.String(), err)
    } else {
        glog.Warningf("rocksdb: address %v hex '%v' reached negative %s %v, resetting to 0", ad, addrDesc, logText, valueSat.String())
    }
    valueSat.SetInt64(0)
}

// GetAndResetConnectBlockStats gets statistics about cache usage in connect blocks and resets the counters
func (d *RocksDB) GetAndResetConnectBlockStats() string {
    s := fmt.Sprintf("%+v", d.cbs)
    d.cbs = connectBlockStats{}
    return s
}

// PIVX
const OP_CHECKCOLDSTAKEVERIFY_LOF = 0xd1
const OP_CHECKCOLDSTAKEVERIFY = 0xd2

func isPayToColdStake(signatureScript []byte) bool {
    return len(signatureScript) > 50 && (signatureScript[4] == OP_CHECKCOLDSTAKEVERIFY || signatureScript[4] == OP_CHECKCOLDSTAKEVERIFY_LOF)
}

func getOwnerFromP2CS(signatureScript []byte) ([]byte, error) {
    OwnerScript := make([]byte, 20)
    copy(OwnerScript, signatureScript[28:49])
    return txscript.NewScriptBuilder().AddOp(txscript.OP_DUP).AddOp(txscript.OP_HASH160).
                            AddData(OwnerScript).AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_CHECKSIG).Script()
}


func (d *RocksDB) processAddressesBitcoinType(block *bchain.Block, addresses addressesMap, txAddressesMap map[string]*TxAddresses, balances map[string]*AddrBalance) error {
    blockTxIDs := make([][]byte, len(block.Txs))
    blockTxAddresses := make([]*TxAddresses, len(block.Txs))
    // first process all outputs so that inputs can refer to txs in this block
    for txi := range block.Txs {
        tx := &block.Txs[txi]
        btxID, err := d.chainParser.PackTxid(tx.Txid)
        if err != nil {
            return err
        }
        blockTxIDs[txi] = btxID
        ta := TxAddresses{Height: block.Height}
        ta.Outputs = make([]TxOutput, len(tx.Vout))
        txAddressesMap[string(btxID)] = &ta
        blockTxAddresses[txi] = &ta
        for i, output := range tx.Vout {
            tao := &ta.Outputs[i]
            tao.ValueSat = output.ValueSat
            addrDesc, err := d.chainParser.GetAddrDescFromVout(&output)
            if err != nil || len(addrDesc) == 0 || len(addrDesc) > maxAddrDescLen {
                if err != nil {
                    // do not log ErrAddressMissing, transactions can be without to address (for example eth contracts)
                    if err != bchain.ErrAddressMissing {
                        glog.Warningf("rocksdb: addrDesc: %v - height %d, tx %v, output %v, error %v", err, block.Height, tx.Txid, output, err)
                    }
                } else {
                    glog.V(1).Infof("rocksdb: height %d, tx %v, vout %v, skipping addrDesc of length %d", block.Height, tx.Txid, i, len(addrDesc))
                }
                continue
            }
            tao.AddrDesc = addrDesc
            vOutputAddrDescriptors := []bchain.AddressDescriptor{addrDesc}
            if isPayToColdStake(addrDesc) {
                ownerDesc, e := getOwnerFromP2CS(addrDesc)
                if e == nil {
                    vOutputAddrDescriptors = append(vOutputAddrDescriptors, ownerDesc)
                }
            }
            for _, oad := range vOutputAddrDescriptors {
                if d.chainParser.IsAddrDescIndexable(oad) {
                    strAddrDesc := string(oad)
                    balance, e := balances[strAddrDesc]
                    if !e {
                        balance, err = d.GetAddrDescBalance(oad, addressBalanceDetailUTXOIndexed)
                        if err != nil {
                            return err
                        }
                        if balance == nil {
                            balance = &AddrBalance{}
                        }
                        balances[strAddrDesc] = balance
                        d.cbs.balancesMiss++
                    } else {
                        d.cbs.balancesHit++
                    }
                    // check for duplicates
                    if balance.hasUtxo(btxID, int32(i)) {
                        continue
                    }
                    balance.BalanceSat.Add(&balance.BalanceSat, &output.ValueSat)
                    balance.addUtxo(&Utxo{
                        BtxID:    btxID,
                        Vout:     int32(i),
                        Height:   block.Height,
                        ValueSat: output.ValueSat,
                    })
                    counted := addToAddressesMap(addresses, strAddrDesc, btxID, int32(i))
                    if !counted {
                        balance.Txs++
                    }
                }
            }
        }
    }
    // process inputs (and sapling Data)
    for txi := range block.Txs {
        tx := &block.Txs[txi]
        spendingTxid := blockTxIDs[txi]
        ta := blockTxAddresses[txi]
        ta.Inputs = make([]TxInput, len(tx.Vin))
        logged := false
        for i, input := range tx.Vin {
            tai := &ta.Inputs[i]
            btxID, err := d.chainParser.PackTxid(input.Txid)
            if err != nil {
                // do not process inputs without input txid
                if err == bchain.ErrTxidMissing {
                    continue
                }
                return err
            }
            stxID := string(btxID)
            ita, e := txAddressesMap[stxID]
            if !e {
                ita, err = d.getTxAddresses(btxID)
                if err != nil {
                    return err
                }
                if ita == nil {
                    // allow parser to process unknown input, some coins may implement special handling, default is to log warning
                    tai.AddrDesc = d.chainParser.GetAddrDescForUnknownInput(tx, i)
                    tai.ValueSat = *d.chainParser.GetValueSatForUnknownInput(tx, i)
                    continue
                }
                txAddressesMap[stxID] = ita
                d.cbs.txAddressesMiss++
            } else {
                d.cbs.txAddressesHit++
            }
            if len(ita.Outputs) <= int(input.Vout) {
                glog.Warningf("rocksdb: height %d, tx %v, input tx %v vout %v is out of bounds of stored tx", block.Height, tx.Txid, input.Txid, input.Vout)
                continue
            }
            spentOutput := &ita.Outputs[int(input.Vout)]
            if spentOutput.Spent {
                glog.Warningf("rocksdb: height %d, tx %v, input tx %v vout %v is double spend", block.Height, tx.Txid, input.Txid, input.Vout)
            }
            tai.AddrDesc = spentOutput.AddrDesc
            tai.ValueSat = spentOutput.ValueSat
            // mark the output as spent in tx
            spentOutput.Spent = true
            if len(spentOutput.AddrDesc) == 0 {
                if !logged {
                    glog.V(1).Infof("rocksdb: height %d, tx %v, input tx %v vout %v skipping empty address", block.Height, tx.Txid, input.Txid, input.Vout)
                    logged = true
                }
                continue
            }
            vOutputAddrDescriptors := []bchain.AddressDescriptor{spentOutput.AddrDesc}
            if isPayToColdStake(spentOutput.AddrDesc) {
                ownerDesc, e := getOwnerFromP2CS(spentOutput.AddrDesc)
                if e == nil {
                    vOutputAddrDescriptors = append(vOutputAddrDescriptors, ownerDesc)
                }
            }
            for _, soad := range vOutputAddrDescriptors {
                if d.chainParser.IsAddrDescIndexable(soad) {
                    strAddrDesc := string(soad)
                    balance, e := balances[strAddrDesc]
                    if !e {
                        balance, err = d.GetAddrDescBalance(soad, addressBalanceDetailUTXOIndexed)
                        if err != nil {
                            return err
                        }
                        if balance == nil {
                            balance = &AddrBalance{}
                        }
                        balances[strAddrDesc] = balance
                        d.cbs.balancesMiss++
                    } else {
                        d.cbs.balancesHit++
                    }
                    counted := addToAddressesMap(addresses, strAddrDesc, spendingTxid, ^int32(i))
                    if !counted {
                        balance.Txs++
                    }
                    balance.BalanceSat.Sub(&balance.BalanceSat, &spentOutput.ValueSat)
                    balance.markUtxoAsSpent(btxID, int32(input.Vout))
                    if balance.BalanceSat.Sign() < 0 {
                        d.resetValueSatToZero(&balance.BalanceSat, soad, "balance")
                    }
                    balance.SentSat.Add(&balance.SentSat, &spentOutput.ValueSat)
                }
            }
        }
        // process sapling data
        ta.ShieldIns = uint32(len(tx.VShieldIn))
        ta.ShieldOuts = uint32(len(tx.VShieldIn))
        ta.ShieldValBal = tx.ShieldValBal
    }
    return nil
}

// addToAddressesMap maintains mapping between addresses and transactions in one block
// the method assumes that outpus in the block are processed before the inputs
// the return value is true if the tx was processed before, to not to count the tx multiple times
func addToAddressesMap(addresses addressesMap, strAddrDesc string, btxID []byte, index int32) bool {
    // check that the address was already processed in this block
    // if not found, it has certainly not been counted
    at, found := addresses[strAddrDesc]
    if found {
        // if the tx is already in the slice, append the index to the array of indexes
        for i, t := range at {
            if bytes.Equal(btxID, t.btxID) {
                at[i].indexes = append(t.indexes, index)
                return true
            }
        }
    }
    addresses[strAddrDesc] = append(at, txIndexes{
        btxID:   btxID,
        indexes: []int32{index},
    })
    return false
}

func (d *RocksDB) storeAddresses(wb *gorocksdb.WriteBatch, height uint32, addresses addressesMap) error {
    for addrDesc, txi := range addresses {
        ba := bchain.AddressDescriptor(addrDesc)
        key := packAddressKey(ba, height)
        val := d.packTxIndexes(txi)
        wb.PutCF(d.cfh[cfAddresses], key, val)
    }
    return nil
}

func (d *RocksDB) storeTxAddresses(wb *gorocksdb.WriteBatch, am map[string]*TxAddresses) error {
    varBuf := make([]byte, maxPackedBigintBytes)
    buf := make([]byte, 1024)
    for txID, ta := range am {
        buf = packTxAddresses(ta, buf, varBuf)
        wb.PutCF(d.cfh[cfTxAddresses], []byte(txID), buf)
    }
    return nil
}

func (d *RocksDB) storeBalances(wb *gorocksdb.WriteBatch, abm map[string]*AddrBalance) error {
    // allocate buffer initial buffer
    buf := make([]byte, 1024)
    varBuf := make([]byte, maxPackedBigintBytes)
    for addrDesc, ab := range abm {
        // balance with 0 transactions is removed from db - happens on disconnect
        if ab == nil || ab.Txs <= 0 {
            wb.DeleteCF(d.cfh[cfAddressBalance], bchain.AddressDescriptor(addrDesc))
        } else {
            buf = packAddrBalance(ab, buf, varBuf)
            wb.PutCF(d.cfh[cfAddressBalance], bchain.AddressDescriptor(addrDesc), buf)
        }
    }
    return nil
}

func (d *RocksDB) cleanupBlockTxs(wb *gorocksdb.WriteBatch, block *bchain.Block) error {
    keep := d.chainParser.KeepBlockAddresses()
    // cleanup old block address
    if block.Height > uint32(keep) {
        for rh := block.Height - uint32(keep); rh > 0; rh-- {
            key := packUint(rh)
            val, err := d.db.GetCF(d.ro, d.cfh[cfBlockTxs], key)
            if err != nil {
                return err
            }
            // nil data means the key was not found in DB
            if val.Data() == nil {
                break
            }
            val.Free()
            d.db.DeleteCF(d.wo, d.cfh[cfBlockTxs], key)
        }
    }
    return nil
}

func (d *RocksDB) storeAndCleanupBlockTxs(wb *gorocksdb.WriteBatch, block *bchain.Block) error {
    pl := d.chainParser.PackedTxidLen()
    buf := make([]byte, 0, pl*len(block.Txs))
    varBuf := make([]byte, vlq.MaxLen64)
    zeroTx := make([]byte, pl)
    for i := range block.Txs {
        tx := &block.Txs[i]
        o := make([]outpoint, len(tx.Vin))
        for v := range tx.Vin {
            vin := &tx.Vin[v]
            btxID, err := d.chainParser.PackTxid(vin.Txid)
            if err != nil {
                // do not process inputs without input txid
                if err == bchain.ErrTxidMissing {
                    btxID = zeroTx
                } else {
                    return err
                }
            }
            o[v].btxID = btxID
            o[v].index = int32(vin.Vout)
        }
        btxID, err := d.chainParser.PackTxid(tx.Txid)
        if err != nil {
            return err
        }
        buf = append(buf, btxID...)
        l := packVaruint(uint(len(o)), varBuf)
        buf = append(buf, varBuf[:l]...)
        buf = append(buf, d.packOutpoints(o)...)
    }
    key := packUint(block.Height)
    wb.PutCF(d.cfh[cfBlockTxs], key, buf)
    return d.cleanupBlockTxs(wb, block)
}

func (d *RocksDB) getBlockTxs(height uint32) ([]blockTxs, error) {
    pl := d.chainParser.PackedTxidLen()
    val, err := d.db.GetCF(d.ro, d.cfh[cfBlockTxs], packUint(height))
    if err != nil {
        return nil, err
    }
    defer val.Free()
    buf := val.Data()
    bt := make([]blockTxs, 0, 8)
    for i := 0; i < len(buf); {
        if len(buf)-i < pl {
            glog.Error("rocksdb: Inconsistent data in blockTxs ", hex.EncodeToString(buf))
            return nil, errors.New("Inconsistent data in blockTxs")
        }
        txid := append([]byte(nil), buf[i:i+pl]...)
        i += pl
        o, ol, err := d.unpackNOutpoints(buf[i:])
        if err != nil {
            glog.Error("rocksdb: Inconsistent data in blockTxs ", hex.EncodeToString(buf))
            return nil, errors.New("Inconsistent data in blockTxs")
        }
        bt = append(bt, blockTxs{
            btxID:  txid,
            inputs: o,
        })
        i += ol
    }
    return bt, nil
}

// GetAddrDescBalance returns AddrBalance for given addrDesc
func (d *RocksDB) GetAddrDescBalance(addrDesc bchain.AddressDescriptor, detail AddressBalanceDetail) (*AddrBalance, error) {
    val, err := d.db.GetCF(d.ro, d.cfh[cfAddressBalance], addrDesc)
    if err != nil {
        return nil, err
    }
    defer val.Free()
    buf := val.Data()
    // 3 is minimum length of addrBalance - 1 byte txs, 1 byte sent, 1 byte balance
    if len(buf) < 3 {
        return nil, nil
    }
    return unpackAddrBalance(buf, d.chainParser.PackedTxidLen(), detail)
}

// GetAddressBalance returns address balance for an address or nil if address not found
func (d *RocksDB) GetAddressBalance(address string, detail AddressBalanceDetail) (*AddrBalance, error) {
    addrDesc, err := d.chainParser.GetAddrDescFromAddress(address)
    if err != nil {
        return nil, err
    }
    return d.GetAddrDescBalance(addrDesc, detail)
}

func (d *RocksDB) getTxAddresses(btxID []byte) (*TxAddresses, error) {
    val, err := d.db.GetCF(d.ro, d.cfh[cfTxAddresses], btxID)
    if err != nil {
        return nil, err
    }
    defer val.Free()
    buf := val.Data()
    // 2 is minimum length of addrBalance - 1 byte height, 1 byte inputs len, 1 byte outputs len
    if len(buf) < 3 {
        return nil, nil
    }
    return unpackTxAddresses(buf)
}

// GetTxAddresses returns TxAddresses for given txid or nil if not found
func (d *RocksDB) GetTxAddresses(txid string) (*TxAddresses, error) {
    btxID, err := d.chainParser.PackTxid(txid)
    if err != nil {
        return nil, err
    }
    return d.getTxAddresses(btxID)
}

// AddrDescForOutpoint defines function that returns address descriptorfor given outpoint or nil if outpoint not found
func (d *RocksDB) AddrDescForOutpoint(outpoint bchain.Outpoint) bchain.AddressDescriptor {
    ta, err := d.GetTxAddresses(outpoint.Txid)
    if err != nil || ta == nil {
        return nil
    }
    if outpoint.Vout < 0 {
        vin := ^outpoint.Vout
        if len(ta.Inputs) <= int(vin) {
            return nil
        }
        return ta.Inputs[vin].AddrDesc
    }
    if len(ta.Outputs) <= int(outpoint.Vout) {
        return nil
    }
    return ta.Outputs[outpoint.Vout].AddrDesc
}

func packTxAddresses(ta *TxAddresses, buf []byte, varBuf []byte) []byte {
    buf = buf[:0]
    l := packVaruint(uint(ta.Height), varBuf)
    buf = append(buf, varBuf[:l]...)
    l = packVaruint(uint(len(ta.Inputs)), varBuf)
    buf = append(buf, varBuf[:l]...)
    for i := range ta.Inputs {
        buf = appendTxInput(&ta.Inputs[i], buf, varBuf)
    }
    l = packVaruint(uint(len(ta.Outputs)), varBuf)
    buf = append(buf, varBuf[:l]...)
    for i := range ta.Outputs {
        buf = appendTxOutput(&ta.Outputs[i], buf, varBuf)
    }
    l = packVaruint(uint(ta.ShieldIns), varBuf)
    buf = append(buf, varBuf[:l]...)
    l = packVaruint(uint(ta.ShieldOuts), varBuf)
    buf = append(buf, varBuf[:l]...)
    l = packBigint(&ta.ShieldValBal, varBuf)
    buf = append(buf, varBuf[:l]...)
    return buf
}

func appendTxInput(txi *TxInput, buf []byte, varBuf []byte) []byte {
    la := len(txi.AddrDesc)
    l := packVaruint(uint(la), varBuf)
    buf = append(buf, varBuf[:l]...)
    buf = append(buf, txi.AddrDesc...)
    l = packBigint(&txi.ValueSat, varBuf)
    buf = append(buf, varBuf[:l]...)
    return buf
}

func appendTxOutput(txo *TxOutput, buf []byte, varBuf []byte) []byte {
    la := len(txo.AddrDesc)
    if txo.Spent {
        la = ^la
    }
    l := packVarint(la, varBuf)
    buf = append(buf, varBuf[:l]...)
    buf = append(buf, txo.AddrDesc...)
    l = packBigint(&txo.ValueSat, varBuf)
    buf = append(buf, varBuf[:l]...)
    return buf
}

func unpackAddrBalance(buf []byte, txidUnpackedLen int, detail AddressBalanceDetail) (*AddrBalance, error) {
    txs, l := unpackVaruint(buf)
    sentSat, sl := unpackBigint(buf[l:])
    balanceSat, bl := unpackBigint(buf[l+sl:])
    l = l + sl + bl
    ab := &AddrBalance{
        Txs:        uint32(txs),
        SentSat:    sentSat,
        BalanceSat: balanceSat,
    }
    if detail != AddressBalanceDetailNoUTXO {
        // estimate the size of utxos to avoid reallocation
        ab.Utxos = make([]Utxo, 0, len(buf[l:])/txidUnpackedLen+3)
        // ab.utxosMap = make(map[string]int, cap(ab.Utxos))
        for len(buf[l:]) >= txidUnpackedLen+3 {
            btxID := append([]byte(nil), buf[l:l+txidUnpackedLen]...)
            l += txidUnpackedLen
            vout, ll := unpackVaruint(buf[l:])
            l += ll
            height, ll := unpackVaruint(buf[l:])
            l += ll
            valueSat, ll := unpackBigint(buf[l:])
            l += ll
            u := Utxo{
                BtxID:    btxID,
                Vout:     int32(vout),
                Height:   uint32(height),
                ValueSat: valueSat,
            }
            if detail == AddressBalanceDetailUTXO {
                ab.Utxos = append(ab.Utxos, u)
            } else {
                ab.addUtxo(&u)
            }
        }
    }
    return ab, nil
}

func packAddrBalance(ab *AddrBalance, buf, varBuf []byte) []byte {
    buf = buf[:0]
    l := packVaruint(uint(ab.Txs), varBuf)
    buf = append(buf, varBuf[:l]...)
    l = packBigint(&ab.SentSat, varBuf)
    buf = append(buf, varBuf[:l]...)
    l = packBigint(&ab.BalanceSat, varBuf)
    buf = append(buf, varBuf[:l]...)
    for _, utxo := range ab.Utxos {
        // if Vout < 0, utxo is marked as spent
        if utxo.Vout >= 0 {
            buf = append(buf, utxo.BtxID...)
            l = packVaruint(uint(utxo.Vout), varBuf)
            buf = append(buf, varBuf[:l]...)
            l = packVaruint(uint(utxo.Height), varBuf)
            buf = append(buf, varBuf[:l]...)
            l = packBigint(&utxo.ValueSat, varBuf)
            buf = append(buf, varBuf[:l]...)
        }
    }
    return buf
}

func unpackTxAddresses(buf []byte) (*TxAddresses, error) {
    ta := TxAddresses{}
    height, l := unpackVaruint(buf)
    ta.Height = uint32(height)
    inputs, ll := unpackVaruint(buf[l:])
    l += ll
    ta.Inputs = make([]TxInput, inputs)
    for i := uint(0); i < inputs; i++ {
        l += unpackTxInput(&ta.Inputs[i], buf[l:])
    }
    outputs, ll := unpackVaruint(buf[l:])
    l += ll
    ta.Outputs = make([]TxOutput, outputs)
    for i := uint(0); i < outputs; i++ {
        l += unpackTxOutput(&ta.Outputs[i], buf[l:])
    }
    shieldinputs, ll := unpackVaruint(buf[l:])
    l += ll
    ta.ShieldIns = uint32(shieldinputs)
    shieldoutputs, ll := unpackVaruint(buf[l:])
    l += ll
    ta.ShieldOuts = uint32(shieldoutputs)
    ta.ShieldValBal, _ = unpackBigint(buf[l:])
    return &ta, nil
}

func unpackTxInput(ti *TxInput, buf []byte) int {
    al, l := unpackVaruint(buf)
    ti.AddrDesc = append([]byte(nil), buf[l:l+int(al)]...)
    al += uint(l)
    ti.ValueSat, l = unpackBigint(buf[al:])
    return l + int(al)
}

func unpackTxOutput(to *TxOutput, buf []byte) int {
    al, l := unpackVarint(buf)
    if al < 0 {
        to.Spent = true
        al = ^al
    }
    to.AddrDesc = append([]byte(nil), buf[l:l+al]...)
    al += l
    to.ValueSat, l = unpackBigint(buf[al:])
    return l + al
}

func (d *RocksDB) packTxIndexes(txi []txIndexes) []byte {
    buf := make([]byte, 0, 32)
    bvout := make([]byte, vlq.MaxLen32)
    // store the txs in reverse order for ordering from newest to oldest
    for j := len(txi) - 1; j >= 0; j-- {
        t := &txi[j]
        buf = append(buf, []byte(t.btxID)...)
        for i, index := range t.indexes {
            index <<= 1
            if i == len(t.indexes)-1 {
                index |= 1
            }
            l := packVarint32(index, bvout)
            buf = append(buf, bvout[:l]...)
        }
    }
    return buf
}

func (d *RocksDB) packOutpoints(outpoints []outpoint) []byte {
    buf := make([]byte, 0, 32)
    bvout := make([]byte, vlq.MaxLen32)
    for _, o := range outpoints {
        l := packVarint32(o.index, bvout)
        buf = append(buf, []byte(o.btxID)...)
        buf = append(buf, bvout[:l]...)
    }
    return buf
}

func (d *RocksDB) unpackNOutpoints(buf []byte) ([]outpoint, int, error) {
    txidUnpackedLen := d.chainParser.PackedTxidLen()
    n, p := unpackVaruint(buf)
    outpoints := make([]outpoint, n)
    for i := uint(0); i < n; i++ {
        if p+txidUnpackedLen >= len(buf) {
            return nil, 0, errors.New("Inconsistent data in unpackNOutpoints")
        }
        btxID := append([]byte(nil), buf[p:p+txidUnpackedLen]...)
        p += txidUnpackedLen
        vout, voutLen := unpackVarint32(buf[p:])
        p += voutLen
        outpoints[i] = outpoint{
            btxID: btxID,
            index: vout,
        }
    }
    return outpoints, p, nil
}

// Block index

// BlockInfo holds information about blocks kept in column height
type BlockInfo struct {
    Hash   string
    Time   int64
    Txs    uint32
    Size   uint32
    Height uint32 // Height is not packed!
}

func (d *RocksDB) packBlockInfo(block *BlockInfo) ([]byte, error) {
    packed := make([]byte, 0, 64)
    varBuf := make([]byte, vlq.MaxLen64)
    b, err := d.chainParser.PackBlockHash(block.Hash)
    if err != nil {
        return nil, err
    }
    packed = append(packed, b...)
    packed = append(packed, packUint(uint32(block.Time))...)
    l := packVaruint(uint(block.Txs), varBuf)
    packed = append(packed, varBuf[:l]...)
    l = packVaruint(uint(block.Size), varBuf)
    packed = append(packed, varBuf[:l]...)
    return packed, nil
}

func (d *RocksDB) unpackBlockInfo(buf []byte) (*BlockInfo, error) {
    pl := d.chainParser.PackedTxidLen()
    // minimum length is PackedTxidLen + 4 bytes time + 1 byte txs + 1 byte size
    if len(buf) < pl+4+2 {
        return nil, nil
    }
    txid, err := d.chainParser.UnpackBlockHash(buf[:pl])
    if err != nil {
        return nil, err
    }
    t := unpackUint(buf[pl:])
    txs, l := unpackVaruint(buf[pl+4:])
    size, _ := unpackVaruint(buf[pl+4+l:])
    return &BlockInfo{
        Hash: txid,
        Time: int64(t),
        Txs:  uint32(txs),
        Size: uint32(size),
    }, nil
}

// GetBestBlock returns the block hash of the block with highest height in the db
func (d *RocksDB) GetBestBlock() (uint32, string, error) {
    it := d.db.NewIteratorCF(d.ro, d.cfh[cfHeight])
    defer it.Close()
    if it.SeekToLast(); it.Valid() {
        bestHeight := unpackUint(it.Key().Data())
        info, err := d.unpackBlockInfo(it.Value().Data())
        if info != nil {
            if glog.V(1) {
                glog.Infof("rocksdb: bestblock %d %+v", bestHeight, info)
            }
            return bestHeight, info.Hash, err
        }
    }
    return 0, "", nil
}

// GetBlockHash returns block hash at given height or empty string if not found
func (d *RocksDB) GetBlockHash(height uint32) (string, error) {
    key := packUint(height)
    val, err := d.db.GetCF(d.ro, d.cfh[cfHeight], key)
    if err != nil {
        return "", err
    }
    defer val.Free()
    info, err := d.unpackBlockInfo(val.Data())
    if info == nil {
        return "", err
    }
    return info.Hash, nil
}

// GetBlockInfo returns block info stored in db
func (d *RocksDB) GetBlockInfo(height uint32) (*BlockInfo, error) {
    key := packUint(height)
    val, err := d.db.GetCF(d.ro, d.cfh[cfHeight], key)
    if err != nil {
        return nil, err
    }
    defer val.Free()
    bi, err := d.unpackBlockInfo(val.Data())
    if err != nil || bi == nil {
        return nil, err
    }
    bi.Height = height
    return bi, err
}

func (d *RocksDB) writeHeightFromBlock(wb *gorocksdb.WriteBatch, block *bchain.Block, op int) error {
    return d.writeHeight(wb, block.Height, &BlockInfo{
        Hash:   block.Hash,
        Time:   block.Time,
        Txs:    uint32(len(block.Txs)),
        Size:   uint32(block.Size),
        Height: block.Height,
    }, op)
}

func (d *RocksDB) writeHeight(wb *gorocksdb.WriteBatch, height uint32, bi *BlockInfo, op int) error {
    key := packUint(height)
    switch op {
    case opInsert:
        val, err := d.packBlockInfo(bi)
        if err != nil {
            return err
        }
        wb.PutCF(d.cfh[cfHeight], key, val)
        d.is.UpdateBestHeight(height)
    case opDelete:
        wb.DeleteCF(d.cfh[cfHeight], key)
        d.is.UpdateBestHeight(height - 1)
    }
    return nil
}

// Disconnect blocks

func (d *RocksDB) disconnectTxAddresses(wb *gorocksdb.WriteBatch, height uint32, btxID []byte, inputs []outpoint, txa *TxAddresses,
    txAddressesToUpdate map[string]*TxAddresses, balances map[string]*AddrBalance) error {
    var err error
    var balance *AddrBalance
    addresses := make(map[string]struct{})
    getAddressBalance := func(addrDesc bchain.AddressDescriptor) (*AddrBalance, error) {
        var err error
        s := string(addrDesc)
        b, fb := balances[s]
        if !fb {
            b, err = d.GetAddrDescBalance(addrDesc, addressBalanceDetailUTXOIndexed)
            if err != nil {
                return nil, err
            }
            balances[s] = b
        }
        return b, nil
    }
    for i, t := range txa.Inputs {
        vInputAddrDescriptors := []bchain.AddressDescriptor{t.AddrDesc}
        if isPayToColdStake(t.AddrDesc) {
            ownerDesc, e := getOwnerFromP2CS(t.AddrDesc)
            if e == nil {
                vInputAddrDescriptors = append(vInputAddrDescriptors, ownerDesc)
            }
        }
        for _, ad := range vInputAddrDescriptors {
            if len(ad) > 0 {
                input := &inputs[i]
                s := string(ad)
                _, exist := addresses[s]
                if !exist {
                    addresses[s] = struct{}{}
                }
                s = string(input.btxID)
                sa, found := txAddressesToUpdate[s]
                if !found {
                    sa, err = d.getTxAddresses(input.btxID)
                    if err != nil {
                        return err
                    }
                    if sa != nil {
                        txAddressesToUpdate[s] = sa
                    }
                }
                var inputHeight uint32
                if sa != nil {
                    sa.Outputs[input.index].Spent = false
                    inputHeight = sa.Height
                }
                if d.chainParser.IsAddrDescIndexable(ad) {
                    balance, err = getAddressBalance(ad)
                    if err != nil {
                        return err
                    }
                    if balance != nil {
                        // subtract number of txs only once
                        if !exist {
                            balance.Txs--
                        }
                        balance.SentSat.Sub(&balance.SentSat, &t.ValueSat)
                        if balance.SentSat.Sign() < 0 {
                            d.resetValueSatToZero(&balance.SentSat, ad, "sent amount")
                        }
                        balance.BalanceSat.Add(&balance.BalanceSat, &t.ValueSat)
                        balance.Utxos = append(balance.Utxos, Utxo{
                            BtxID:    input.btxID,
                            Vout:     input.index,
                            Height:   inputHeight,
                            ValueSat: t.ValueSat,
                        })
                    } else {
                        ad, _, _ := d.chainParser.GetAddressesFromAddrDesc(ad)
                        glog.Warningf("Balance for address %s (%s) not found", ad, ad)
                    }
                }
            }
        }
    }
    for i, t := range txa.Outputs {
        vOutputAddrDescriptors := []bchain.AddressDescriptor{t.AddrDesc}
        if isPayToColdStake(t.AddrDesc) {
            ownerDesc, e := getOwnerFromP2CS(t.AddrDesc)
            if e == nil {
                vOutputAddrDescriptors = append(vOutputAddrDescriptors, ownerDesc)
            }
        }
        for _, ad := range vOutputAddrDescriptors {
            if len(ad) > 0 {
                s := string(ad)
                _, exist := addresses[s]
                if !exist {
                    addresses[s] = struct{}{}
                }
                if d.chainParser.IsAddrDescIndexable(ad) {
                    balance, err := getAddressBalance(ad)
                    if err != nil {
                        return err
                    }
                    if balance != nil {
                        // subtract number of txs only once
                        if !exist {
                            balance.Txs--
                        }
                        balance.BalanceSat.Sub(&balance.BalanceSat, &t.ValueSat)
                        if balance.BalanceSat.Sign() < 0 {
                            d.resetValueSatToZero(&balance.BalanceSat, ad, "balance")
                        }
                        balance.markUtxoAsSpent(btxID, int32(i))
                    } else {
                        ad, _, _ := d.chainParser.GetAddressesFromAddrDesc(ad)
                        glog.Warningf("Balance for address %s (%s) not found", ad, ad)
                    }
                }
            }
        }
    }
    for a := range addresses {
        key := packAddressKey([]byte(a), height)
        wb.DeleteCF(d.cfh[cfAddresses], key)
    }
    return nil
}

// DisconnectBlockRangeBitcoinType removes all data belonging to blocks in range lower-higher
// it is able to disconnect only blocks for which there are data in the blockTxs column
func (d *RocksDB) DisconnectBlockRangeBitcoinType(lower uint32, higher uint32) error {
    blocks := make([][]blockTxs, higher-lower+1)
    for height := lower; height <= higher; height++ {
        blockTxs, err := d.getBlockTxs(height)
        if err != nil {
            return err
        }
        if len(blockTxs) == 0 {
            return errors.Errorf("Cannot disconnect blocks with height %v and lower. It is necessary to rebuild index.", height)
        }
        blocks[height-lower] = blockTxs
    }
    wb := gorocksdb.NewWriteBatch()
    defer wb.Destroy()
    txAddressesToUpdate := make(map[string]*TxAddresses)
    txsToDelete := make(map[string]struct{})
    balances := make(map[string]*AddrBalance)
    for height := higher; height >= lower; height-- {
        blockTxs := blocks[height-lower]
        glog.Info("Disconnecting block ", height, " containing ", len(blockTxs), " transactions")
        // go backwards to avoid interim negative balance
        // when connecting block, amount is first in tx on the output side, then in another tx on the input side
        // when disconnecting, it must be done backwards
        for i := len(blockTxs) - 1; i >= 0; i-- {
            btxID := blockTxs[i].btxID
            s := string(btxID)
            txsToDelete[s] = struct{}{}
            txa, err := d.getTxAddresses(btxID)
            if err != nil {
                return err
            }
            if txa == nil {
                ut, _ := d.chainParser.UnpackTxid(btxID)
                glog.Warning("TxAddress for txid ", ut, " not found")
                continue
            }
            if err := d.disconnectTxAddresses(wb, height, btxID, blockTxs[i].inputs, txa, txAddressesToUpdate, balances); err != nil {
                return err
            }
        }
        key := packUint(height)
        wb.DeleteCF(d.cfh[cfBlockTxs], key)
        wb.DeleteCF(d.cfh[cfHeight], key)
    }
    d.storeTxAddresses(wb, txAddressesToUpdate)
    d.storeBalancesDisconnect(wb, balances)
    for s := range txsToDelete {
        b := []byte(s)
        wb.DeleteCF(d.cfh[cfTransactions], b)
        wb.DeleteCF(d.cfh[cfTxAddresses], b)
    }
    err := d.db.Write(d.wo, wb)
    if err == nil {
        glog.Infof("rocksdb: blocks %d-%d disconnected", lower, higher)
    }
    return err
}

func (d *RocksDB) storeBalancesDisconnect(wb *gorocksdb.WriteBatch, balances map[string]*AddrBalance) {
    for _, b := range balances {
        if b != nil {
            // remove spent utxos
            us := make([]Utxo, 0, len(b.Utxos))
            for _, u := range b.Utxos {
                // remove utxos marked as spent
                if u.Vout >= 0 {
                    us = append(us, u)
                }
            }
            b.Utxos = us
            // sort utxos by height
            sort.SliceStable(b.Utxos, func(i, j int) bool {
                return b.Utxos[i].Height < b.Utxos[j].Height
            })
        }
    }
    d.storeBalances(wb, balances)

}
func dirSize(path string) (int64, error) {
    var size int64
    err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
        if err == nil {
            if !info.IsDir() {
                size += info.Size()
            }
        }
        return err
    })
    return size, err
}

// DatabaseSizeOnDisk returns size of the database in bytes
func (d *RocksDB) DatabaseSizeOnDisk() int64 {
    size, err := dirSize(d.path)
    if err != nil {
        glog.Warning("rocksdb: DatabaseSizeOnDisk: ", err)
        return 0
    }
    return size
}

// GetTx returns transaction stored in db and height of the block containing it
func (d *RocksDB) GetTx(txid string) (*bchain.Tx, uint32, error) {
    key, err := d.chainParser.PackTxid(txid)
    if err != nil {
        return nil, 0, err
    }
    val, err := d.db.GetCF(d.ro, d.cfh[cfTransactions], key)
    if err != nil {
        return nil, 0, err
    }
    defer val.Free()
    data := val.Data()
    if len(data) > 4 {
        return d.chainParser.UnpackTx(data)
    }
    return nil, 0, nil
}

// PutTx stores transactions in db
func (d *RocksDB) PutTx(tx *bchain.Tx, height uint32, blockTime int64) error {
    key, err := d.chainParser.PackTxid(tx.Txid)
    if err != nil {
        return nil
    }
    buf, err := d.chainParser.PackTx(tx, height, blockTime)
    if err != nil {
        return err
    }
    err = d.db.PutCF(d.wo, d.cfh[cfTransactions], key, buf)
    if err == nil {
        d.is.AddDBColumnStats(cfTransactions, 1, int64(len(key)), int64(len(buf)))
    }
    return err
}

// DeleteTx removes transactions from db
func (d *RocksDB) DeleteTx(txid string) error {
    key, err := d.chainParser.PackTxid(txid)
    if err != nil {
        return nil
    }
    // use write batch so that this delete matches other deletes
    wb := gorocksdb.NewWriteBatch()
    defer wb.Destroy()
    d.internalDeleteTx(wb, key)
    return d.db.Write(d.wo, wb)
}

// internalDeleteTx checks if tx is cached and updates internal state accordingly
func (d *RocksDB) internalDeleteTx(wb *gorocksdb.WriteBatch, key []byte) {
    val, err := d.db.GetCF(d.ro, d.cfh[cfTransactions], key)
    // ignore error, it is only for statistics
    if err == nil {
        l := len(val.Data())
        if l > 0 {
            d.is.AddDBColumnStats(cfTransactions, -1, int64(-len(key)), int64(-l))
        }
        defer val.Free()
    }
    wb.DeleteCF(d.cfh[cfTransactions], key)
}

// internal state
const internalStateKey = "internalState"

// LoadInternalState loads from db internal state or initializes a new one if not yet stored
func (d *RocksDB) LoadInternalState(rpcCoin string) (*common.InternalState, error) {
    val, err := d.db.GetCF(d.ro, d.cfh[cfDefault], []byte(internalStateKey))
    if err != nil {
        return nil, err
    }
    defer val.Free()
    data := val.Data()
    var is *common.InternalState
    if len(data) == 0 {
        is = &common.InternalState{Coin: rpcCoin}
    } else {
        is, err = common.UnpackInternalState(data)
        if err != nil {
            return nil, err
        }
        // verify that the rpc coin matches DB coin
        // running it mismatched would corrupt the database
        if is.Coin == "" {
            is.Coin = rpcCoin
        } else if is.Coin != rpcCoin {
            return nil, errors.Errorf("Coins do not match. DB coin %v, RPC coin %v", is.Coin, rpcCoin)
        }
    }
    // make sure that column stats match the columns
    sc := is.DbColumns
    nc := make([]common.InternalStateColumn, len(cfNames))
    for i := 0; i < len(nc); i++ {
        nc[i].Name = cfNames[i]
        nc[i].Version = dbVersion
        for j := 0; j < len(sc); j++ {
            if sc[j].Name == nc[i].Name {
                // check the version of the column, if it does not match, the db is not compatible
                if sc[j].Version != dbVersion {
                    return nil, errors.Errorf("DB version %v of column '%v' does not match the required version %v. DB is not compatible.", sc[j].Version, sc[j].Name, dbVersion)
                }
                nc[i].Rows = sc[j].Rows
                nc[i].KeyBytes = sc[j].KeyBytes
                nc[i].ValueBytes = sc[j].ValueBytes
                nc[i].Updated = sc[j].Updated
                break
            }
        }
    }
    is.DbColumns = nc
    // after load, reset the synchronization data
    is.IsSynchronized = false
    is.IsMempoolSynchronized = false
    var t time.Time
    is.LastMempoolSync = t
    is.SyncMode = false
    return is, nil
}

// SetInconsistentState sets the internal state to DbStateInconsistent or DbStateOpen based on inconsistent parameter
// db in left in DbStateInconsistent state cannot be used and must be recreated
func (d *RocksDB) SetInconsistentState(inconsistent bool) error {
    if d.is == nil {
        return errors.New("Internal state not created")
    }
    if inconsistent {
        d.is.DbState = common.DbStateInconsistent
    } else {
        d.is.DbState = common.DbStateOpen
    }
    return d.storeState(d.is)
}

// SetInternalState sets the InternalState to be used by db to collect internal state
func (d *RocksDB) SetInternalState(is *common.InternalState) {
    d.is = is
}

// StoreInternalState stores the internal state to db
func (d *RocksDB) StoreInternalState(is *common.InternalState) error {
    if d.metrics != nil {
        for c := 0; c < len(cfNames); c++ {
            rows, keyBytes, valueBytes := d.is.GetDBColumnStatValues(c)
            d.metrics.DbColumnRows.With(common.Labels{"column": cfNames[c]}).Set(float64(rows))
            d.metrics.DbColumnSize.With(common.Labels{"column": cfNames[c]}).Set(float64(keyBytes + valueBytes))
        }
    }
    return d.storeState(is)
}

func (d *RocksDB) storeState(is *common.InternalState) error {
    buf, err := is.Pack()
    if err != nil {
        return err
    }
    return d.db.PutCF(d.wo, d.cfh[cfDefault], []byte(internalStateKey), buf)
}

func (d *RocksDB) computeColumnSize(col int, stopCompute chan os.Signal) (int64, int64, int64, error) {
    var rows, keysSum, valuesSum int64
    var seekKey []byte
    // do not use cache
    ro := gorocksdb.NewDefaultReadOptions()
    ro.SetFillCache(false)
    for {
        var key []byte
        it := d.db.NewIteratorCF(ro, d.cfh[col])
        if rows == 0 {
            it.SeekToFirst()
        } else {
            glog.Info("db: Column ", cfNames[col], ": rows ", rows, ", key bytes ", keysSum, ", value bytes ", valuesSum, ", in progress...")
            it.Seek(seekKey)
            it.Next()
        }
        for count := 0; it.Valid() && count < refreshIterator; it.Next() {
            select {
            case <-stopCompute:
                return 0, 0, 0, errors.New("Interrupted")
            default:
            }
            key = it.Key().Data()
            count++
            rows++
            keysSum += int64(len(key))
            valuesSum += int64(len(it.Value().Data()))
        }
        seekKey = append([]byte{}, key...)
        valid := it.Valid()
        it.Close()
        if !valid {
            break
        }
    }
    return rows, keysSum, valuesSum, nil
}

// ComputeInternalStateColumnStats computes stats of all db columns and sets them to internal state
// can be very slow operation
func (d *RocksDB) ComputeInternalStateColumnStats(stopCompute chan os.Signal) error {
    start := time.Now()
    glog.Info("db: ComputeInternalStateColumnStats start")
    for c := 0; c < len(cfNames); c++ {
        rows, keysSum, valuesSum, err := d.computeColumnSize(c, stopCompute)
        if err != nil {
            return err
        }
        d.is.SetDBColumnStats(c, rows, keysSum, valuesSum)
        glog.Info("db: Column ", cfNames[c], ": rows ", rows, ", key bytes ", keysSum, ", value bytes ", valuesSum)
    }
    glog.Info("db: ComputeInternalStateColumnStats finished in ", time.Since(start))
    return nil
}

// Helpers

func packAddressKey(addrDesc bchain.AddressDescriptor, height uint32) []byte {
    buf := make([]byte, len(addrDesc)+packedHeightBytes)
    copy(buf, addrDesc)
    // pack height as binary complement to achieve ordering from newest to oldest block
    binary.BigEndian.PutUint32(buf[len(addrDesc):], ^height)
    return buf
}

func unpackAddressKey(key []byte) ([]byte, uint32, error) {
    i := len(key) - packedHeightBytes
    if i <= 0 {
        return nil, 0, errors.New("Invalid address key")
    }
    // height is packed in binary complement, convert it
    return key[:i], ^unpackUint(key[i : i+packedHeightBytes]), nil
}

func packUint(i uint32) []byte {
    buf := make([]byte, 4)
    binary.BigEndian.PutUint32(buf, i)
    return buf
}

func unpackUint(buf []byte) uint32 {
    return binary.BigEndian.Uint32(buf)
}

func packVarint32(i int32, buf []byte) int {
    return vlq.PutInt(buf, int64(i))
}

func packVarint(i int, buf []byte) int {
    return vlq.PutInt(buf, int64(i))
}

func packVaruint(i uint, buf []byte) int {
    return vlq.PutUint(buf, uint64(i))
}

func unpackVarint32(buf []byte) (int32, int) {
    i, ofs := vlq.Int(buf)
    return int32(i), ofs
}

func unpackVarint(buf []byte) (int, int) {
    i, ofs := vlq.Int(buf)
    return int(i), ofs
}

func unpackVaruint(buf []byte) (uint, int) {
    i, ofs := vlq.Uint(buf)
    return uint(i), ofs
}

const (
    // number of bits in a big.Word
    wordBits = 32 << (uint64(^big.Word(0)) >> 63)
    // number of bytes in a big.Word
    wordBytes = wordBits / 8
    // max packed bigint words
    maxPackedBigintWords = (256 - wordBytes) / wordBytes
    maxPackedBigintBytes = 249
)

// big int is packed in BigEndian order without memory allocation as 1 byte length followed by bytes of big int
// number of written bytes is returned
// limitation: bigints longer than 248 bytes are truncated to 248 bytes
// caution: buffer must be big enough to hold the packed big int, buffer 249 bytes big is always safe
func packBigint(bi *big.Int, buf []byte) int {
    w := bi.Bits()
    lw := len(w)
    // zero returns only one byte - zero length
    if lw == 0 {
        buf[0] = 0
        return 1
    }
    // pack the most significant word in a special way - skip leading zeros
    w0 := w[lw-1]
    fb := 8
    mask := big.Word(0xff) << (wordBits - 8)
    for w0&mask == 0 {
        fb--
        mask >>= 8
    }
    for i := fb; i > 0; i-- {
        buf[i] = byte(w0)
        w0 >>= 8
    }
    // if the big int is too big (> 2^1984), the number of bytes would not fit to 1 byte
    // in this case, truncate the number, it is not expected to work with this big numbers as amounts
    s := 0
    if lw > maxPackedBigintWords {
        s = lw - maxPackedBigintWords
    }
    // pack the rest of the words in reverse order
    for j := lw - 2; j >= s; j-- {
        d := w[j]
        for i := fb + wordBytes; i > fb; i-- {
            buf[i] = byte(d)
            d >>= 8
        }
        fb += wordBytes
    }
    buf[0] = byte(fb)
    return fb + 1
}

func unpackBigint(buf []byte) (big.Int, int) {
    var r big.Int
    l := int(buf[0]) + 1
    r.SetBytes(buf[1:l])
    return r, l
}
