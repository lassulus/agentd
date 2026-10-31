package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// ACP is JSON-RPC 2.0 over newline-delimited JSON on stdio. Params and
// results stay json.RawMessage so agent payloads reach the history unchanged.

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("%s (%d): %s", e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("%s (%d)", e.Message, e.Code)
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcHandler answers requests and notifications from the peer. For
// notifications the return values are ignored and the handler must not
// block. Requests run in their own goroutine, so they may block (e.g. until
// a user answers a permission).
type rpcHandler func(method string, params json.RawMessage) (any, *rpcError)

type rpcConn struct {
	w       io.Writer
	handler rpcHandler

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan rpcMessage

	done chan struct{}
	err  error
}

func newRPCConn(r io.Reader, w io.Writer, handler rpcHandler) *rpcConn {
	c := &rpcConn{w: w, handler: handler, pending: map[string]chan rpcMessage{}, done: make(chan struct{})}
	go c.readLoop(r)
	return c
}

func (c *rpcConn) readLoop(r io.Reader) {
	br := bufio.NewReader(r)
	var err error
	for {
		var line []byte
		line, err = br.ReadBytes('\n')
		if len(line) > 1 {
			var m rpcMessage
			if jerr := json.Unmarshal(line, &m); jerr == nil {
				c.dispatch(m)
			}
		}
		if err != nil {
			break
		}
	}
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *rpcConn) dispatch(m rpcMessage) {
	if m.Method == "" {
		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		delete(c.pending, string(m.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
		return
	}
	if len(m.ID) == 0 {
		// Notifications run in read order: session/update chunks must reach
		// the history in the order the agent sent them, and before the
		// response that ends the turn.
		c.handler(m.Method, m.Params)
		return
	}
	go func() {
		res, rerr := c.handler(m.Method, m.Params)
		out := rpcMessage{JSONRPC: "2.0", ID: m.ID, Error: rerr}
		if rerr == nil {
			b, err := json.Marshal(res)
			if err != nil {
				out.Error = &rpcError{Code: -32603, Message: err.Error()}
			} else {
				out.Result = b
			}
		}
		_ = c.send(out)
	}()
}

func (c *rpcConn) send(m rpcMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

var errConnClosed = errors.New("agent connection closed")

// Call sends a request and waits for its response. A nil result discards it.
func (c *rpcConn) Call(ctx context.Context, method string, params any, result any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.pending == nil || isClosed(c.done) {
		c.mu.Unlock()
		return errConnClosed
	}
	c.nextID++
	id := json.RawMessage(strconv.FormatInt(c.nextID, 10))
	ch := make(chan rpcMessage, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()

	if err := c.send(rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: p}); err != nil {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return errConnClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *rpcConn) Notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(rpcMessage{JSONRPC: "2.0", Method: method, Params: p})
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
