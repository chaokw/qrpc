package qrpc

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
)

/* api provides utilities for make nonblocking api calls with qrpc.Connection */

// API for non blocking roundtrip calls
type API interface {
	Call(ctx context.Context, cmd Cmd, payload []byte) (*Frame, error)
	CallOne(ctx context.Context, endpoint string, cmd Cmd, payload []byte) (*Frame, error)
	CallAll(ctx context.Context, cmd Cmd, payload []byte) map[string]*APIResult
}

// NewAPI creates an API instance
func NewAPI(endpoints []string, conf ConnectionConfig, weights []int) API {
	var totalWeight int
	if weights == nil {
		weights = make([]int, len(endpoints))
		for idx := range weights {
			weights[idx] = 1
		}
		totalWeight = len(endpoints)
	} else {
		for _, weight := range weights {
			totalWeight += weight
		}
	}

	ep := make([]string, len(endpoints))
	copy(ep, endpoints)

	idxMap := make(map[string]int)
	d := &defaultAPI{endpoints: ep, weights: weights, totalWeight: totalWeight, conf: conf}

	for idx, endpoint := range endpoints {
		idxMap[endpoint] = idx
		conn, err := NewConnection(endpoint, conf, nil)
		if err != nil {
			logError("NewConnection fail", endpoint, err)
			continue
		}
		d.conns.Store(idx, conn)
		d.activeConns.Store(conn, idx)
	}
	d.idxMap = idxMap

	return d
}

type defaultAPI struct {
	// imutable
	endpoints   []string
	weights     []int
	idxMap      map[string]int
	totalWeight int
	conf        ConnectionConfig

	activeConns sync.Map // map[*Connection]int
	conns       sync.Map // map[int]*Connection
	mu          sync.Mutex
}

// call with random endpoint
func (api *defaultAPI) Call(ctx context.Context, cmd Cmd, payload []byte) (result *Frame, err error) {

	idx := api.getIdx()
	result, err = api.callViaIdx(ctx, idx, cmd, payload)
	if err != nil {
		result, err = api.callViaActiveConns(ctx, cmd, payload)
	}

	return
}

// APIResult is response for each endpoint
type APIResult struct {
	Frame *Frame
	Err   error
}

func (api *defaultAPI) CallAll(ctx context.Context, cmd Cmd, payload []byte) map[string]*APIResult {

	result := make(map[string]*APIResult)

	var wg sync.WaitGroup
	for i := range api.endpoints {
		idx := i
		GoFunc(&wg, func() {
			frame, err := api.callViaIdx(ctx, idx, cmd, payload)
			result[api.endpoints[idx]] = &APIResult{Frame: frame, Err: err}
		})
	}
	wg.Wait()

	return result
}

var (
	// ErrEndPointNotExists when call non exist endpoint
	ErrEndPointNotExists = errors.New("endpoint not exists")
	// ErrEndPointNotAvaiable when specified endpoint not available
	ErrEndPointNotAvaiable = errors.New("endpoint not avaiable")
)

func (api *defaultAPI) CallOne(ctx context.Context, endpoint string, cmd Cmd, payload []byte) (*Frame, error) {
	idx, ok := api.idxMap[endpoint]
	if !ok {
		return nil, ErrEndPointNotExists
	}

	return api.callViaIdx(ctx, idx, cmd, payload)
}

func (api *defaultAPI) callViaIdx(ctx context.Context, idx int, cmd Cmd, payload []byte) (result *Frame, err error) {
	c, ok := api.conns.Load(idx)
	if !ok {
		return api.callWithoutConn(ctx, idx, cmd, payload)
	}

	conn := c.(*Connection)
	_, resp, err := conn.Request(cmd, NBFlag, payload)
	if err != nil {
		api.safeCloseDeleteConn(idx, conn)

		return api.callWithoutConn(ctx, idx, cmd, payload)
	}
	return resp.GetFrameWithContext(ctx)
}

func (api *defaultAPI) callWithoutConn(ctx context.Context, idx int, cmd Cmd, payload []byte) (result *Frame, err error) {
	api.mu.Lock()
	c, ok := api.conns.Load(idx)

	if !ok {
		conn := api.reconnectIdx(idx)
		if conn == nil {
			return nil, ErrEndPointNotAvaiable
		}
		api.safeStoreConnLocked(idx, conn)
		api.mu.Unlock()
		_, resp, err := conn.Request(cmd, NBFlag, payload)
		if err != nil {
			api.safeCloseDeleteConn(idx, conn)
			return nil, err
		}
		return resp.GetFrameWithContext(ctx)
	}

	api.mu.Unlock()

	conn := c.(*Connection)
	_, resp, err := conn.Request(cmd, NBFlag, payload)
	if err != nil {
		api.safeCloseDeleteConn(idx, conn)

		return nil, err
	}

	return resp.GetFrameWithContext(ctx)
}

func (api *defaultAPI) safeCloseDeleteConn(idx int, conn *Connection) {
	conn.Close()
	api.mu.Lock()
	defer api.mu.Unlock()

	c, ok := api.conns.Load(idx)
	if ok && c.(*Connection) == conn {
		api.conns.Delete(idx)
	}
	api.activeConns.Delete(conn)
}

func (api *defaultAPI) safeStoreConnLocked(idx int, conn *Connection) {
	_, ok := api.conns.Load(idx)
	if ok {
		panic(fmt.Sprintf("bug when safeStoreConnLocked:%d", idx))
	}
	api.conns.Store(idx, conn)
	api.activeConns.Store(conn, idx)
}

func (api *defaultAPI) reconnectIdx(idx int) *Connection {
	conn, err := NewConnection(api.endpoints[idx], api.conf, nil)
	if err != nil {
		logError("NewConnection fail", err)
		return nil
	}

	return conn
}

func (api *defaultAPI) callViaActiveConns(ctx context.Context, cmd Cmd, payload []byte) (result *Frame, err error) {
	api.activeConns.Range(func(k, v interface{}) bool {
		ac := k.(*Connection)
		_, resp, err := ac.Request(cmd, NBFlag, payload)
		if err != nil {
			idx := v.(int)
			api.safeCloseDeleteConn(idx, ac)
			result, err = api.callWithoutConn(ctx, idx, cmd, payload)
		} else {
			result, err = resp.GetFrameWithContext(ctx)
		}

		return false
	})

	return
}

func (api *defaultAPI) getIdx() int {
	targetWeight := rand.Intn(api.totalWeight)
	sumWeight := 0
	for idx, weight := range api.weights {
		sumWeight += weight
		if sumWeight > targetWeight {
			return idx
		}
	}

	logError("getIdx bug")
	return 0
}
