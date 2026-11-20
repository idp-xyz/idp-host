package harness

// D53: the calls of one step that only read run at the same time. The rule that picks them (groupEnd, concurrentSafe), the
// way a group runs (executeGroup) and the way its answers reach the thread (commitInOrder) are here; what one call comes to
// is decided by invoke, the same function the one-at-a-time path uses.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
)

// DefaultParallelCalls is how many read-only calls of a step run at once when a gateway's configuration does not say.
const DefaultParallelCalls = 4

// parallelLimit is the most calls of one step this gateway takes at once from one run; 1 means one at a time.
func (g *Gateway) parallelLimit() int {
	if g.ParallelCalls <= 0 {
		return DefaultParallelCalls
	}
	return g.ParallelCalls
}

// concurrentSafe reports whether a call may run beside other calls (D53): it is automatic (CONFIRMED: no confirmation is
// pending, it was not denied, it is not the caller's, it is not a sub-agent), and it is a gateway call that the gateway
// itself lists as read-only (readOnlyHint, not merely idempotent: two idempotent writes can still conflict) and that the
// operator did not name in sequentialTools, or the HOST's own resource__read.
func (t *turn) concurrentSafe(tc run.ToolCall) bool {
	if tc.State != run.CallConfirmed {
		return false
	}
	if _, ok := t.subagents[tc.Tool]; ok {
		return false
	}
	if tc.Tool == agent.ResourceReadTool && t.resources != nil {
		return true
	}
	return t.gateway != nil && t.readOnly[tc.Tool] && !agent.MatchesTool(t.gateway.SequentialTools, tc.Tool)
}

// groupEnd returns the end (exclusive) of the group of calls that begins at records[i], or i when there is none. A group is
// consecutive calls that are all concurrentSafe, at least two of them; a call that was denied before it started (the
// policy, D51, the step's limits) does not split it, since it only waits to be answered in its place. Anything else
// (a call that waits for the member, a sub-agent, the caller's own tool, a call that may write) ends it.
func (t *turn) groupEnd(records []run.ToolCall, i int) int {
	if t.gateway == nil || t.gateway.parallelLimit() < 2 || !t.concurrentSafe(records[i]) {
		return i
	}
	n, last := 0, i
scan:
	for k := i; k < len(records); k++ {
		switch {
		case t.concurrentSafe(records[k]):
			n++
			last = k
		case records[k].State == run.CallDenied:
		default:
			break scan
		}
	}
	if n < 2 {
		return i
	}
	return last + 1
}

// executeGroup runs the calls of a group at once, at most the gateway's parallelLimit in flight, and answers them in the
// order the model asked for them: a call's answer is written when it and every call before it are finished, so the thread
// and the event log read as if the calls had run one after the other, and a worker that dies in between leaves the first
// calls answered and the others EXECUTING or CONFIRMED, which D47 handles (a read-only call is simply sent again).
//
// It returns errInterrupted when the deadline or a cancel reached the group (every call is recorded: finished ones with
// their result, calls in flight as interrupted, calls not started as not run), errStopped when the worker was told to stop
// and some call was not started (it rests CONFIRMED), and otherwise the error that ends the turn, as execute does.
func (t *turn) executeGroup(calls []run.ToolCall) error {
	if t.stopping() {
		return errStopped
	}
	t.ensureLimits()
	if err := t.loadTickets(); err != nil { // before any call reads t.tickets
		return err
	}
	gctx, abort := context.WithCancelCause(t.callCtx)
	defer abort(nil)
	n := len(calls)
	outs := make([]callOutcome, n)
	ready := make([]chan struct{}, n)
	for i := range ready {
		ready[i] = make(chan struct{})
	}
	committed := make(chan error, 1)
	go func() { committed <- t.commitInOrder(calls, outs, ready, abort) }()

	sem := make(chan struct{}, t.gateway.parallelLimit())
	var wg sync.WaitGroup
	var launchErr error
	skipFrom := n
launch:
	for i, tc := range calls {
		if tc.State == run.CallDenied {
			outs[i] = callOutcome{state: run.CallDenied, errText: tc.Result}
			close(ready[i])
			continue
		}
		if t.stopping() || gctx.Err() != nil {
			skipFrom = i // nothing new is started (D47), and nothing after a deadline or a cancel (D46)
			break
		}
		if tc.Tool == agent.ResourceReadTool && t.resources != nil {
			state, result, errText := t.resourceResult(tc)
			outs[i] = callOutcome{state: state, result: result, errText: errText, ok: state == run.CallDone}
			close(ready[i])
			continue
		}
		args, delivers, refusal, err := t.fileArguments(tc)
		if err != nil {
			launchErr, skipFrom = err, i
			break
		}
		if refusal != "" {
			outs[i] = callOutcome{state: run.CallFailed, errText: refusal}
			close(ready[i])
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-gctx.Done():
			skipFrom = i
			break launch
		case <-t.stop: // nil when there is no worker: never ready
			skipFrom = i
			break launch
		}
		if t.stopping() || gctx.Err() != nil { // a slot and the stop at the same time: the stop wins
			<-sem
			skipFrom = i
			break
		}
		wg.Add(1)
		go func(i int, tc run.ToolCall, args json.RawMessage, delivers bool) {
			defer wg.Done()
			o := callOutcome{}
			if err := t.h.Store.UpdateToolCall(t.ctx, t.claim, tc.ID, run.CallExecuting, "", false, nil); err != nil {
				o.err = err
			} else {
				o = t.invoke(gctx, tc, args, delivers)
			}
			<-sem
			outs[i] = o
			close(ready[i])
		}(i, tc, args, delivers)
	}
	for i := skipFrom; i < n; i++ {
		outs[i].skipped = true
		close(ready[i])
	}
	err := <-committed
	abort(errors.New("the group is over"))
	wg.Wait()
	if err != nil {
		return err
	}
	return launchErr
}

// commitInOrder writes the calls' answers in the order of the calls, each as soon as it and the calls before it are ready:
// the call's final state, its tool message, its sources and its events in one transaction (D47, D49). It returns what the
// group came to (see executeGroup).
func (t *turn) commitInOrder(calls []run.ToolCall, outs []callOutcome, ready []chan struct{}, abort context.CancelCauseFunc) error {
	interrupted := false
	for i, tc := range calls {
		<-ready[i]
		o := outs[i]
		switch {
		case o.skipped:
			// the calls from here on were not started
			return t.unstarted(calls[i:], interrupted)
		case o.interrupted:
			if err := t.interruptedCall(&tc); !errors.Is(err, errInterrupted) {
				abort(err)
				return err
			}
			interrupted = true
		default:
			if _, err := t.settleOutcome(tc, o); err != nil {
				abort(err)
				return err
			}
		}
	}
	if interrupted {
		return errInterrupted
	}
	return nil
}

// unstarted handles the calls of a group that were not started: after the deadline or a cancel they are answered as not
// run, like the calls after an interrupted one are when they run one at a time (D46); after a stop they rest as they are
// for the next worker (D47).
func (t *turn) unstarted(calls []run.ToolCall, interrupted bool) error {
	switch {
	case t.callCtx.Err() != nil:
		if !t.ours() {
			return errInterrupted
		}
		for _, tc := range calls {
			var err error
			switch tc.State {
			case run.CallConfirmed:
				err = t.answer(tc, run.CallFailed, "", "not run: "+t.interruptReason()+" before this call started")
			case run.CallDenied:
				err = t.answer(tc, run.CallDenied, "", "not run: the run ended before this call was handled")
			}
			if err != nil {
				return err
			}
		}
		return errInterrupted
	case interrupted:
		return errInterrupted
	case t.stopping():
		return errStopped
	}
	return nil // the group was given up for another reason, which its caller returns
}

// inCallOrder puts the calls that are still to be handled in the order the model asked for them (the order of the
// assistant message's calls): the store lists them by step and creation time, which is the same instant for all of a step's
// calls, so a call id decides. It is used when calls can run together, so that what is left of a step after a crash is
// answered in the thread in the same order as the rest of it.
func (t *turn) inCallOrder(calls []run.ToolCall) []run.ToolCall {
	pos := map[string]int{}
	for _, m := range t.rc.Messages {
		if m.Role != agui.RoleAssistant {
			continue
		}
		for i, c := range m.ToolCalls {
			pos[c.ID] = i
		}
	}
	out := append([]run.ToolCall(nil), calls...)
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Step != out[b].Step {
			return out[a].Step < out[b].Step
		}
		return pos[out[a].ID] < pos[out[b].ID]
	})
	return out
}
