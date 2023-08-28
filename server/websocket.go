package server

import (
	"blockbook/api"
	"blockbook/bchain"
	"blockbook/common"
	"blockbook/db"
	"encoding/json"
	"math/big"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/gorilla/websocket"
	"github.com/juju/errors"
)

const upgradeFailed = "Upgrade failed: "
const outChannelSize = 500
const defaultTimeout = 60 * time.Second

var (
	// ErrorMethodNotAllowed is returned when client tries to upgrade method other than GET
	ErrorMethodNotAllowed = errors.New("Method not allowed")

	connectionCounter uint64
)

type websocketReq struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type websocketRes struct {
	ID   string      `json:"id"`
	Data interface{} `json:"data"`
}

type websocketChannel struct {
	id            uint64
	conn          *websocket.Conn
	out           chan *websocketRes
	ip            string
	requestHeader http.Header
	alive         bool
	aliveLock     sync.Mutex
}

// WebsocketServer is a handle to websocket server
type WebsocketServer struct {
	socket                    *websocket.Conn
	upgrader                  *websocket.Upgrader
	db                        *db.RocksDB
	txCache                   *db.TxCache
	chain                     bchain.BlockChain
	chainParser               bchain.BlockChainParser
	mempool                   bchain.Mempool
	metrics                   *common.Metrics
	is                        *common.InternalState
	api                       *api.Worker
	block0hash                string
	newBlockSubscriptions     map[*websocketChannel]string
	newBlockSubscriptionsLock sync.Mutex
	addressSubscriptions      map[string]map[*websocketChannel]string
	addressSubscriptionsLock  sync.Mutex
}

// NewWebsocketServer creates new websocket interface to blockbook and returns its handle
func NewWebsocketServer(db *db.RocksDB, chain bchain.BlockChain, mempool bchain.Mempool, txCache *db.TxCache, metrics *common.Metrics, is *common.InternalState) (*WebsocketServer, error) {
	api, err := api.NewWorker(db, chain, mempool, txCache, is)
	if err != nil {
		return nil, err
	}
	b0, err := db.GetBlockHash(0)
	if err != nil {
		return nil, err
	}
	s := &WebsocketServer{
		upgrader: &websocket.Upgrader{
			ReadBufferSize:  1024 * 32,
			WriteBufferSize: 1024 * 32,
			CheckOrigin:     checkOrigin,
		},
		db:                    db,
		txCache:               txCache,
		chain:                 chain,
		chainParser:           chain.GetChainParser(),
		mempool:               mempool,
		metrics:               metrics,
		is:                    is,
		api:                   api,
		block0hash:            b0,
		newBlockSubscriptions: make(map[*websocketChannel]string),
		addressSubscriptions:  make(map[string]map[*websocketChannel]string),
	}
	return s, nil
}

// allow all origins
func checkOrigin(r *http.Request) bool {
	return true
}

// ServeHTTP sets up handler of websocket channel
func (s *WebsocketServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, upgradeFailed+ErrorMethodNotAllowed.Error(), 503)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, upgradeFailed+err.Error(), 503)
		return
	}
	c := &websocketChannel{
		id:            atomic.AddUint64(&connectionCounter, 1),
		conn:          conn,
		out:           make(chan *websocketRes, outChannelSize),
		ip:            r.RemoteAddr,
		requestHeader: r.Header,
		alive:         true,
	}
	go s.inputLoop(c)
	go s.outputLoop(c)
	s.onConnect(c)
}

// GetHandler returns http handler
func (s *WebsocketServer) GetHandler() http.Handler {
	return s
}

func (s *WebsocketServer) closeChannel(c *websocketChannel) {
	c.aliveLock.Lock()
	defer c.aliveLock.Unlock()
	if c.alive {
		c.conn.Close()
		c.alive = false
		//clean out
		close(c.out)
		for len(c.out) > 0 {
			<-c.out
		}
		s.onDisconnect(c)
	}
}

func (c *websocketChannel) IsAlive() bool {
	c.aliveLock.Lock()
	defer c.aliveLock.Unlock()
	return c.alive
}

func (s *WebsocketServer) inputLoop(c *websocketChannel) {
	defer func() {
		if r := recover(); r != nil {
			glog.Error("recovered from panic: ", r, ", ", c.id)
			debug.PrintStack()
			s.closeChannel(c)
		}
	}()
	for {
		t, d, err := c.conn.ReadMessage()
		if err != nil {
			s.closeChannel(c)
			return
		}
		switch t {
		case websocket.TextMessage:
			var req websocketReq
			err := json.Unmarshal(d, &req)
			if err != nil {
				glog.Error("Error parsing message from ", c.id, ", ", string(d), ", ", err)
				s.closeChannel(c)
				return
			}
			go s.onRequest(c, &req)
		case websocket.BinaryMessage:
			glog.Error("Binary message received from ", c.id, ", ", c.ip)
			s.closeChannel(c)
			return
		case websocket.PingMessage:
			c.conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(defaultTimeout))
			break
		case websocket.CloseMessage:
			s.closeChannel(c)
			return
		case websocket.PongMessage:
			// do nothing
		}
	}
}

func (s *WebsocketServer) outputLoop(c *websocketChannel) {
	for m := range c.out {
		err := c.conn.WriteJSON(m)
		if err != nil {
			glog.Error("Error sending message to ", c.id, ", ", err)
			s.closeChannel(c)
		}
	}
}

func (s *WebsocketServer) onConnect(c *websocketChannel) {
	glog.Info("Client connected ", c.id, ", ", c.ip)
	s.metrics.WebsocketClients.Inc()
}

func (s *WebsocketServer) onDisconnect(c *websocketChannel) {
	s.unsubscribeNewBlock(c)
	s.unsubscribeAddresses(c)
	glog.Info("Client disconnected ", c.id, ", ", c.ip)
	s.metrics.WebsocketClients.Dec()
}

var requestHandlers = map[string]func(*WebsocketServer, *websocketChannel, *websocketReq) (interface{}, error){
	"getAccountInfo": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		r, err := unmarshalGetAccountInfoRequest(req.Params)
		if err == nil {
			rv, err = s.getAccountInfo(r)
		}
		return
	},
	"getInfo": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		return s.getInfo()
	},
	"getBlockHash": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		r := struct {
			Height int `json:"height"`
		}{}
		err = json.Unmarshal(req.Params, &r)
		if err == nil {
			rv, err = s.getBlockHash(r.Height)
		}
		return
	},
	"getAccountUtxo": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		r := struct {
			Descriptor string `json:"descriptor"`
		}{}
		err = json.Unmarshal(req.Params, &r)
		if err == nil {
			rv, err = s.getAccountUtxo(r.Descriptor)
		}
		return
	},
	"getTransaction": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		r := struct {
			Txid string `json:"txid"`
		}{}
		err = json.Unmarshal(req.Params, &r)
		if err == nil {
			rv, err = s.getTransaction(r.Txid)
		}
		return
	},
	"getTransactionSpecific": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		r := struct {
			Txid string `json:"txid"`
		}{}
		err = json.Unmarshal(req.Params, &r)
		if err == nil {
			rv, err = s.getTransactionSpecific(r.Txid)
		}
		return
	},
	"estimateFee": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		return s.estimateFee(c, req.Params)
	},
	"sendTransaction": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		r := struct {
			Hex string `json:"hex"`
		}{}
		err = json.Unmarshal(req.Params, &r)
		if err == nil {
			rv, err = s.sendTransaction(r.Hex)
		}
		return
	},
	"subscribeNewBlock": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		return s.subscribeNewBlock(c, req)
	},
	"unsubscribeNewBlock": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		return s.unsubscribeNewBlock(c)
	},
	"subscribeAddresses": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		ad, err := s.unmarshalAddresses(req.Params)
		if err == nil {
			rv, err = s.subscribeAddresses(c, ad, req)
		}
		return
	},
	"unsubscribeAddresses": func(s *WebsocketServer, c *websocketChannel, req *websocketReq) (rv interface{}, err error) {
		return s.unsubscribeAddresses(c)
	},
}

func sendResponse(c *websocketChannel, req *websocketReq, data interface{}) {
	defer func() {
		if r := recover(); r != nil {
			glog.Error("Client ", c.id, ", onRequest ", req.Method, " recovered from panic: ", r)
		}
	}()
	c.out <- &websocketRes{
		ID:   req.ID,
		Data: data,
	}
}

func (s *WebsocketServer) onRequest(c *websocketChannel, req *websocketReq) {
	var err error
	var data interface{}
	defer func() {
		if r := recover(); r != nil {
			glog.Error("Client ", c.id, ", onRequest ", req.Method, " recovered from panic: ", r)
			debug.PrintStack()
			e := resultError{}
			e.Error.Message = "Internal error"
			data = e
		}
		// nil data means no response
		if data != nil {
			sendResponse(c, req, data)
		}
	}()
	t := time.Now()
	defer s.metrics.WebsocketReqDuration.With(common.Labels{"method": req.Method}).Observe(float64(time.Since(t)) / 1e3) // in microseconds
	f, ok := requestHandlers[req.Method]
	if ok {
		data, err = f(s, c, req)
	} else {
		err = errors.New("unknown method")
	}
	if err == nil {
		glog.V(1).Info("Client ", c.id, " onRequest ", req.Method, " success")
		s.metrics.WebsocketRequests.With(common.Labels{"method": req.Method, "status": "success"}).Inc()
	} else {
		glog.Error("Client ", c.id, " onMessage ", req.Method, ": ", errors.ErrorStack(err))
		s.metrics.WebsocketRequests.With(common.Labels{"method": req.Method, "status": err.Error()}).Inc()
		e := resultError{}
		e.Error.Message = err.Error()
		data = e
	}
}

type accountInfoReq struct {
	Descriptor     string `json:"descriptor"`
	Details        string `json:"details"`
	Tokens         string `json:"tokens"`
	PageSize       int    `json:"pageSize"`
	Page           int    `json:"page"`
	FromHeight     int    `json:"from"`
	ToHeight       int    `json:"to"`
	ContractFilter string `json:"contractFilter"`
}

func unmarshalGetAccountInfoRequest(params []byte) (*accountInfoReq, error) {
	var r accountInfoReq
	err := json.Unmarshal(params, &r)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *WebsocketServer) getAccountInfo(req *accountInfoReq) (res *api.Address, err error) {
	var opt api.AccountDetails
	switch req.Details {
	case "tokens":
		opt = api.AccountDetailsTokens
	case "tokenBalances":
		opt = api.AccountDetailsTokenBalances
	case "txids":
		opt = api.AccountDetailsTxidHistory
	case "txs":
		opt = api.AccountDetailsTxHistory
	default:
		opt = api.AccountDetailsBasic
	}
	var tokensToReturn api.TokensToReturn
	switch req.Tokens {
	case "used":
		tokensToReturn = api.TokensToReturnUsed
	case "nonzero":
		tokensToReturn = api.TokensToReturnNonzeroBalance
	default:
		tokensToReturn = api.TokensToReturnDerived
	}
	filter := api.AddressFilter{
		FromHeight:     uint32(req.FromHeight),
		ToHeight:       uint32(req.ToHeight),
		Contract:       req.ContractFilter,
		Vout:           api.AddressFilterVoutOff,
		TokensToReturn: tokensToReturn,
	}
	if req.PageSize == 0 {
		req.PageSize = txsOnPage
	}
	a, err := s.api.GetXpubAddress(req.Descriptor, req.Page, req.PageSize, opt, &filter, 0)
	if err != nil {
		return s.api.GetAddress(req.Descriptor, req.Page, req.PageSize, opt, &filter)
	}
	return a, nil
}

func (s *WebsocketServer) getAccountUtxo(descriptor string) (interface{}, error) {
	utxo, err := s.api.GetXpubUtxo(descriptor, false, 0)
	if err != nil {
		return s.api.GetAddressUtxo(descriptor, false)
	}
	return utxo, nil
}

func (s *WebsocketServer) getTransaction(txid string) (interface{}, error) {
	return s.api.GetTransaction(txid, false, false)
}

func (s *WebsocketServer) getTransactionSpecific(txid string) (interface{}, error) {
	return s.chain.GetTransactionSpecific(&bchain.Tx{Txid: txid})
}

func (s *WebsocketServer) getInfo() (interface{}, error) {
	vi := common.GetVersionInfo()
	height, hash, err := s.db.GetBestBlock()
	if err != nil {
		return nil, err
	}
	type info struct {
		Name       string `json:"name"`
		Shortcut   string `json:"shortcut"`
		Decimals   int    `json:"decimals"`
		Version    string `json:"version"`
		BestHeight int    `json:"bestHeight"`
		BestHash   string `json:"bestHash"`
		Block0Hash string `json:"block0Hash"`
		Testnet    bool   `json:"testnet"`
	}
	return &info{
		Name:       s.is.Coin,
		Shortcut:   s.is.CoinShortcut,
		Decimals:   s.chainParser.AmountDecimals(),
		BestHeight: int(height),
		BestHash:   hash,
		Version:    vi.Version,
		Block0Hash: s.block0hash,
		Testnet:    s.chain.IsTestnet(),
	}, nil
}

func (s *WebsocketServer) getBlockHash(height int) (interface{}, error) {
	h, err := s.db.GetBlockHash(uint32(height))
	if err != nil {
		return nil, err
	}
	type hash struct {
		Hash string `json:"hash"`
	}
	return &hash{
		Hash: h,
	}, nil
}

func (s *WebsocketServer) estimateFee(c *websocketChannel, params []byte) (interface{}, error) {
	type estimateFeeReq struct {
		Blocks   []int                  `json:"blocks"`
		Specific map[string]interface{} `json:"specific"`
	}
	type estimateFeeRes struct {
		FeePerTx   string `json:"feePerTx,omitempty"`
		FeePerUnit string `json:"feePerUnit,omitempty"`
		FeeLimit   string `json:"feeLimit,omitempty"`
	}
	var r estimateFeeReq
	err := json.Unmarshal(params, &r)
	if err != nil {
		return nil, err
	}
	res := make([]estimateFeeRes, len(r.Blocks))
	if s.chainParser.GetChainType() == bchain.ChainEthereumType {
		gas, err := s.chain.EthereumTypeEstimateGas(r.Specific)
		if err != nil {
			return nil, err
		}
		sg := strconv.FormatUint(gas, 10)
		for i, b := range r.Blocks {
			fee, err := s.chain.EstimateSmartFee(b, true)
			if err != nil {
				return nil, err
			}
			res[i].FeePerUnit = fee.String()
			res[i].FeeLimit = sg
			fee.Mul(&fee, new(big.Int).SetUint64(gas))
			res[i].FeePerTx = fee.String()
		}
	} else {
		conservative := true
		v, ok := r.Specific["conservative"]
		if ok {
			vc, ok := v.(bool)
			if ok {
				conservative = vc
			}
		}
		txSize := 0
		v, ok = r.Specific["txsize"]
		if ok {
			f, ok := v.(float64)
			if ok {
				txSize = int(f)
			}
		}
		for i, b := range r.Blocks {
			fee, err := s.chain.EstimateSmartFee(b, conservative)
			if err != nil {
				return nil, err
			}
			res[i].FeePerUnit = fee.String()
			if txSize > 0 {
				fee.Mul(&fee, big.NewInt(int64(txSize)))
				fee.Add(&fee, big.NewInt(500))
				fee.Div(&fee, big.NewInt(1000))
				res[i].FeePerTx = fee.String()
			}
		}
	}
	return res, nil
}

func (s *WebsocketServer) sendTransaction(tx string) (res resultSendTransaction, err error) {
	txid, err := s.chain.SendRawTransaction(tx)
	if err != nil {
		return res, err
	}
	res.Result = txid
	return
}

type subscriptionResponse struct {
	Subscribed bool `json:"subscribed"`
}

func (s *WebsocketServer) subscribeNewBlock(c *websocketChannel, req *websocketReq) (res interface{}, err error) {
	s.newBlockSubscriptionsLock.Lock()
	defer s.newBlockSubscriptionsLock.Unlock()
	s.newBlockSubscriptions[c] = req.ID
	return &subscriptionResponse{true}, nil
}

func (s *WebsocketServer) unsubscribeNewBlock(c *websocketChannel) (res interface{}, err error) {
	s.newBlockSubscriptionsLock.Lock()
	defer s.newBlockSubscriptionsLock.Unlock()
	delete(s.newBlockSubscriptions, c)
	return &subscriptionResponse{false}, nil
}

func (s *WebsocketServer) unmarshalAddresses(params []byte) ([]bchain.AddressDescriptor, error) {
	r := struct {
		Addresses []string `json:"addresses"`
	}{}
	err := json.Unmarshal(params, &r)
	if err != nil {
		return nil, err
	}
	rv := make([]bchain.AddressDescriptor, len(r.Addresses))
	for i, a := range r.Addresses {
		ad, err := s.chainParser.GetAddrDescFromAddress(a)
		if err != nil {
			return nil, err
		}
		rv[i] = ad
	}
	return rv, nil
}

func (s *WebsocketServer) subscribeAddresses(c *websocketChannel, addrDesc []bchain.AddressDescriptor, req *websocketReq) (res interface{}, err error) {
	// unsubscribe all previous subscriptions
	s.unsubscribeAddresses(c)
	s.addressSubscriptionsLock.Lock()
	defer s.addressSubscriptionsLock.Unlock()
	for i := range addrDesc {
		ads := string(addrDesc[i])
		as, ok := s.addressSubscriptions[ads]
		if !ok {
			as = make(map[*websocketChannel]string)
			s.addressSubscriptions[ads] = as
		}
		as[c] = req.ID
	}
	return &subscriptionResponse{true}, nil
}

// unsubscribeAddresses unsubscribes all address subscriptions by this channel
func (s *WebsocketServer) unsubscribeAddresses(c *websocketChannel) (res interface{}, err error) {
	s.addressSubscriptionsLock.Lock()
	defer s.addressSubscriptionsLock.Unlock()
	for _, sa := range s.addressSubscriptions {
		for sc := range sa {
			if sc == c {
				delete(sa, c)
			}
		}
	}
	return &subscriptionResponse{false}, nil
}

// OnNewBlock is a callback that broadcasts info about new block to subscribed clients
func (s *WebsocketServer) OnNewBlock(hash string, height uint32) {
	s.newBlockSubscriptionsLock.Lock()
	defer s.newBlockSubscriptionsLock.Unlock()
	data := struct {
		Height uint32 `json:"height"`
		Hash   string `json:"hash"`
	}{
		Height: height,
		Hash:   hash,
	}
	for c, id := range s.newBlockSubscriptions {
		if c.IsAlive() {
			c.out <- &websocketRes{
				ID:   id,
				Data: &data,
			}
		}
	}
	glog.Info("broadcasting new block ", height, " ", hash, " to ", len(s.newBlockSubscriptions), " channels")
}

// OnNewTxAddr is a callback that broadcasts info about a tx affecting subscribed address
func (s *WebsocketServer) OnNewTxAddr(tx *bchain.Tx, addrDesc bchain.AddressDescriptor) {
	// check if there is any subscription but release the lock immediately, GetTransactionFromBchainTx may take some time
	s.addressSubscriptionsLock.Lock()
	as, ok := s.addressSubscriptions[string(addrDesc)]
	s.addressSubscriptionsLock.Unlock()
	if ok && len(as) > 0 {
		addr, _, err := s.chainParser.GetAddressesFromAddrDesc(addrDesc)
		if err != nil {
			glog.Error("GetAddressesFromAddrDesc error ", err, " for ", addrDesc)
			return
		}
		if len(addr) == 1 {
			atx, err := s.api.GetTransactionFromBchainTx(tx, 0, false, false)
			if err != nil {
				glog.Error("GetTransactionFromBchainTx error ", err, " for ", tx.Txid)
				return
			}
			data := struct {
				Address string  `json:"address"`
				Tx      *api.Tx `json:"tx"`
			}{
				Address: addr[0],
				Tx:      atx,
			}
			// get the list of subscriptions again, this time keep the lock
			s.addressSubscriptionsLock.Lock()
			defer s.addressSubscriptionsLock.Unlock()
			as, ok = s.addressSubscriptions[string(addrDesc)]
			if ok {
				for c, id := range as {
					if c.IsAlive() {
						c.out <- &websocketRes{
							ID:   id,
							Data: &data,
						}
					}
				}
				glog.Info("broadcasting new tx ", tx.Txid, " for addr ", addr[0], " to ", len(as), " channels")
			}
		}
	}
}
