package serve

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// acpConn is the client side of an Agent Client Protocol connection: JSON-RPC
// 2.0, one message per line over the agent's stdin and stdout (F8). The
// service is the client; the agent sends it session updates and requests
// (permissions, elicitations).
type acpConn struct {
	mu      sync.Mutex // writes
	w       io.Writer
	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[int64]chan acpReply
	closed  bool
	// handler receives the agent's requests (reply with conn.reply) and
	// notifications (id nil).
	handler func(method string, id json.RawMessage, params json.RawMessage)
}

type acpReply struct {
	Result json.RawMessage
	Err    *acpError
}

type acpError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *acpError) Error() string {
	if len(e.Data) > 0 && string(e.Data) != "null" {
		return fmt.Sprintf("%s (%d): %s", e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("%s (%d)", e.Message, e.Code)
}

// errACPClosed: the agent's output ended before the reply.
var errACPClosed = errors.New("the agent process ended")

type acpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpError       `json:"error,omitempty"`
}

func newACPConn(w io.Writer, handler func(string, json.RawMessage, json.RawMessage)) *acpConn {
	return &acpConn{w: w, pending: map[int64]chan acpReply{}, handler: handler}
}

func (c *acpConn) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

// call sends a request and returns a channel for its reply.
func (c *acpConn) call(method string, params any) (<-chan acpReply, error) {
	id := c.nextID.Add(1)
	ch := make(chan acpReply, 1)
	c.pmu.Lock()
	if c.closed {
		c.pmu.Unlock()
		return nil, errACPClosed
	}
	c.pending[id] = ch
	c.pmu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
		return nil, err
	}
	return ch, nil
}

// request sends a request and waits for its reply, decoded into out.
func (c *acpConn) request(method string, params any, out any) error {
	ch, err := c.call(method, params)
	if err != nil {
		return err
	}
	rep, ok := <-ch
	if !ok {
		return errACPClosed
	}
	if rep.Err != nil {
		return fmt.Errorf("%s: %w", method, rep.Err)
	}
	if out != nil && len(rep.Result) > 0 {
		if err := json.Unmarshal(rep.Result, out); err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
	}
	return nil
}

func (c *acpConn) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// reply answers an agent's request.
func (c *acpConn) reply(id json.RawMessage, result any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *acpConn) replyError(id json.RawMessage, code int, msg string) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

// read dispatches the agent's output until it ends; pending calls then
// fail with errACPClosed.
func (c *acpConn) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			break
		}
	}
	c.pmu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.pmu.Unlock()
}

func (c *acpConn) dispatch(line []byte) {
	var m acpMessage
	if json.Unmarshal(line, &m) != nil || m.JSONRPC != "2.0" {
		return // not a protocol line (a log the agent printed)
	}
	if m.Method != "" {
		c.handler(m.Method, m.ID, m.Params)
		return
	}
	var id int64
	if json.Unmarshal(m.ID, &id) != nil {
		return
	}
	c.pmu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.pmu.Unlock()
	if ch != nil {
		ch <- acpReply{Result: m.Result, Err: m.Error}
	}
}
