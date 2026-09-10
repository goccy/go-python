package internal

// Callback dispatch: the Python -> Go half of the function bridge. The guest
// reaches the host through the single wasmify callback_invoke import; the
// generated Module routes it to the CallbackHandler registered under the
// callback id the instance handed the guest via py_set_go_dispatcher.

import (
	"fmt"

	wasm2go "github.com/goccy/pythonwasm2go"
)

// dispatcher is the instance's CallbackHandler: every method id is a
// user-bound function id, forwarded to the public package's handler.
type dispatcher struct{ p *Python }

func (d dispatcher) HandleCallback(methodID int32, req []byte) ([]byte, error) {
	if d.p.UserHandler == nil {
		return nil, fmt.Errorf("python: no Go function handler installed")
	}
	return d.p.UserHandler(methodID, req)
}

// EnsureDispatcher registers this instance's callback handler with the
// generated bridge machinery and tells the guest its callback id. Runs once
// per instance.
func (p *Python) EnsureDispatcher() error {
	p.dispMu.Lock()
	defer p.dispMu.Unlock()
	if p.dispatcherSet {
		return nil
	}
	if p.closed.Load() {
		return ErrClosed
	}

	// Register on this instance's Module (NOT a process-global registry —
	// callback registries are per-module host state).
	p.m.cbMu.Lock()
	if p.m.callbacks == nil {
		p.m.callbacks = map[int32]CallbackHandler{}
	}
	p.m.nextCBID++
	cbID := p.m.nextCBID
	p.m.callbacks[cbID] = dispatcher{p: p}
	p.m.cbMu.Unlock()

	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendInt32(buf, 2, cbID)
	resp, err := p.m.invoke(0, midSetGoDispatcher, buf, wasm2go.Inv_0_31)
	if err != nil {
		return fmt.Errorf("set Go dispatcher: %w", err)
	}
	if e := pbExtractError(resp); e != nil {
		return fmt.Errorf("set Go dispatcher: %w", e)
	}
	p.dispatcherSet = true
	return nil
}
