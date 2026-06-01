// Agent — SDK control channel for human contact-center agents in Go.
//
// Mirrors the TypeScript SDK's Agent class. Opens a single WSS
// connection to the gateway's /agent/control endpoint, authenticates
// with a short-lived BFF-minted JWT, and streams JSON frames in both
// directions. Slice 1 surface (states: offline | ready | aux | on_call)
// per project_agent_presence_design.md.
//
// Headless callers (CLI, integration tests, server-side agent harness)
// use this directly. The browser/Vite path lives in the TS SDK.
//
// Lives in its own package so the cgo-heavy `pkg/client.go` doesn't
// drag the C ffi link requirement into headless agent builds.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// AgentState mirrors clutchcall::shared::agent_presence::state on the
// gateway. Wire format is lowercase ASCII — keep these constants
// byte-identical with the C++ to_string output.
type AgentState string

const (
	StateOffline AgentState = "offline"
	StateReady   AgentState = "ready"
	StateAux     AgentState = "aux"
	StateOnCall  AgentState = "on_call"
	StateAcw     AgentState = "acw"
	StateRinging AgentState = "ringing"
)

// AgentOptions configures a single Agent control channel.
type AgentOptions struct {
	// WssURL is the wss:// endpoint returned by the BFF's
	// agentPresence.openControlSession proc.
	WssURL string
	// Token is the short-lived HS256 JWT the gateway will validate.
	Token string
	// HeartbeatInterval defaults to 5s. Don't set lower than 1s —
	// the gateway's disconnect-grace watchdog (Slice 2) uses this
	// cadence as its reference.
	HeartbeatInterval time.Duration
	// DialTimeout caps the initial WSS upgrade. Defaults to 10s.
	DialTimeout time.Duration
}

// AgentEvent is emitted on the channel returned by Agent.Events().
// Exactly one of the fields is populated per event.
type AgentEvent struct {
	Welcome            *WelcomeEvent
	Ack                *AckEvent
	Err                *ErrorEvent
	ForcedLogout       *ForcedLogoutEvent
	StateChanged       *StateChangedEvent
	Closed             *ClosedEvent
	// Slice 2 — call routing push frames.
	CallOffered        *CallOfferedEvent
	CallOfferedExpired *CallOfferedExpiredEvent
	CallConfirmed      *CallConfirmedEvent
}

type WelcomeEvent struct {
	DeviceID string
}
type AckEvent struct {
	State AgentState
	TS    int64
}
type ErrorEvent struct {
	Code    string
	Message string
}
type ForcedLogoutEvent struct {
	Reason string
}
type StateChangedEvent struct {
	State     AgentState
	AuxReason string
	TS        int64
}
type ClosedEvent struct {
	Clean  bool
	Reason string
}

// CallOfferedEvent fires when the gateway routes a call to this agent.
// Consumers MUST call Confirm or Reject before RingUntilMs elapses; past
// the deadline the gateway emits CallOfferedExpired and may force aux:rona.
type CallOfferedEvent struct {
	CallID      string
	RingUntilMs int64
}

// CallOfferedExpiredEvent fires when the ring deadline passed before a
// confirm/reject was received. ForcedRona == true when the missed-offer
// counter crossed the gateway's threshold (the gateway has already
// moved this agent into aux:rona).
type CallOfferedExpiredEvent struct {
	CallID      string
	MissedCount int64
	ForcedRona  bool
}

// CallConfirmedEvent fires after Confirm round-trips. The agent is now
// in state on_call.
type CallConfirmedEvent struct {
	CallID string
}

// Agent owns a single WSS control channel.
type Agent struct {
	opts AgentOptions

	conn   *websocket.Conn
	connMu sync.Mutex // serialises writes; gorilla/ws is not concurrent-write safe

	// State the read loop maintains. Atomic so SetState callers can
	// read currentState without holding connMu.
	currentState atomic.Value // AgentState
	deviceID     atomic.Value // string
	// Slice 2 — last unresolved call offer the gateway pushed. nil when
	// no offer is in flight. Loaded as *CallOfferedEvent.
	pendingOffer atomic.Value

	// Slice 2 — confirm/reject ack routing. confirmPending is keyed by
	// call_id; the read loop hands the resolution back via a one-shot
	// channel. Concurrent confirms on the same call_id are not expected
	// (UI gates that), but the slice keeps multiple to stay symmetric
	// with `pending`.
	confirmPending   map[string][]chan confirmOrErr
	confirmPendingMu sync.Mutex

	events chan AgentEvent

	// Outstanding setState calls register here so the read loop can
	// hand the matching ack back. Keyed by target state — the
	// gateway echoes the state in its ack, so we route by state.
	// One outstanding setState per state at a time is fine because
	// Slice 1 transitions are SDK-driven and serialized by the
	// operator's UI.
	pending   map[AgentState][]chan ackOrErr
	pendingMu sync.Mutex

	closeOnce sync.Once
	doneCh    chan struct{}
	hbCancel  context.CancelFunc
}

type ackOrErr struct {
	ack AckEvent
	err *ErrorEvent
}

type confirmOrErr struct {
	confirmed CallConfirmedEvent
	err       *ErrorEvent
}

// NewAgent constructs an Agent. Nothing is dialed until Connect.
func NewAgent(opts AgentOptions) *Agent {
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = 5 * time.Second
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 10 * time.Second
	}
	a := &Agent{
		opts:           opts,
		events:         make(chan AgentEvent, 32),
		pending:        make(map[AgentState][]chan ackOrErr),
		confirmPending: make(map[string][]chan confirmOrErr),
		doneCh:         make(chan struct{}),
	}
	a.currentState.Store(StateOffline)
	a.deviceID.Store("")
	a.pendingOffer.Store((*CallOfferedEvent)(nil))
	return a
}

// PendingOffer returns the last unresolved call offer the gateway
// pushed, or nil when no offer is in flight. Cleared on Confirm /
// Reject success and on CallOfferedExpired.
func (a *Agent) PendingOffer() *CallOfferedEvent {
	v, _ := a.pendingOffer.Load().(*CallOfferedEvent)
	return v
}

// Confirm accepts an offered call. Blocks until the gateway emits
// call_confirmed for the matching call_id or returns an error frame.
// When callID is empty, accepts the currently-pending offer.
func (a *Agent) Confirm(ctx context.Context, callID string) (CallConfirmedEvent, error) {
	if a.conn == nil {
		return CallConfirmedEvent{}, errors.New("agent: Confirm called before Connect")
	}
	if callID == "" {
		if p := a.PendingOffer(); p != nil {
			callID = p.CallID
		}
	}
	if callID == "" {
		return CallConfirmedEvent{}, errors.New("agent: Confirm: no call_id and no pending offer")
	}
	ackCh := make(chan confirmOrErr, 1)
	a.confirmPendingMu.Lock()
	a.confirmPending[callID] = append(a.confirmPending[callID], ackCh)
	a.confirmPendingMu.Unlock()
	defer func() {
		a.confirmPendingMu.Lock()
		if list := a.confirmPending[callID]; len(list) > 0 {
			for i, ch := range list {
				if ch == ackCh {
					a.confirmPending[callID] = append(list[:i], list[i+1:]...)
					break
				}
			}
			if len(a.confirmPending[callID]) == 0 {
				delete(a.confirmPending, callID)
			}
		}
		a.confirmPendingMu.Unlock()
	}()
	if err := a.writeJSON(map[string]any{"type": "confirm", "call_id": callID}); err != nil {
		return CallConfirmedEvent{}, fmt.Errorf("agent: writeJSON failed: %w", err)
	}
	select {
	case r := <-ackCh:
		if r.err != nil {
			return CallConfirmedEvent{}, fmt.Errorf("%s: %s", r.err.Code, r.err.Message)
		}
		a.currentState.Store(StateOnCall)
		a.pendingOffer.Store((*CallOfferedEvent)(nil))
		return r.confirmed, nil
	case <-a.doneCh:
		return CallConfirmedEvent{}, errors.New("agent: control channel closed mid-confirm")
	case <-ctx.Done():
		return CallConfirmedEvent{}, ctx.Err()
	}
}

// Reject declines an offered call. The gateway returns the agent to
// ready and bumps the missed-offer counter; threshold triggers
// aux:rona. Reject is fire-and-forget — the gateway emits
// state_changed asynchronously which lands on Events().
func (a *Agent) Reject(callID string) error {
	if a.conn == nil {
		return errors.New("agent: Reject called before Connect")
	}
	if callID == "" {
		if p := a.PendingOffer(); p != nil {
			callID = p.CallID
		}
	}
	if callID == "" {
		return errors.New("agent: Reject: no call_id and no pending offer")
	}
	a.pendingOffer.Store((*CallOfferedEvent)(nil))
	return a.writeJSON(map[string]any{"type": "reject", "call_id": callID})
}

// Events returns a channel that emits server-initiated events
// (welcome, forced_logout, state_changed, error, close). Acks for
// SetState are NOT delivered on this channel — they're routed back
// to the SetState caller directly. Consumers must drain this
// channel; if it fills the read loop blocks on event dispatch.
func (a *Agent) Events() <-chan AgentEvent { return a.events }

// CurrentState returns the last state the server ack'd. Defaults to
// offline before the first successful SetState.
func (a *Agent) CurrentState() AgentState {
	if s, ok := a.currentState.Load().(AgentState); ok {
		return s
	}
	return StateOffline
}

// DeviceID returns the server-assigned device id, available after
// Connect returns. Empty before the welcome frame arrives.
func (a *Agent) DeviceID() string {
	if s, ok := a.deviceID.Load().(string); ok {
		return s
	}
	return ""
}

// Connect opens the WSS upgrade, waits for the welcome frame, and
// kicks off the read + heartbeat loops. Returns when the welcome
// frame has been received, or on error.
func (a *Agent) Connect(ctx context.Context) error {
	u, err := url.Parse(a.opts.WssURL)
	if err != nil {
		return fmt.Errorf("agent: bad WssURL %q: %w", a.opts.WssURL, err)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: a.opts.DialTimeout,
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+a.opts.Token)

	dctx, cancel := context.WithTimeout(ctx, a.opts.DialTimeout)
	defer cancel()

	conn, _, err := dialer.DialContext(dctx, u.String(), hdr)
	if err != nil {
		return fmt.Errorf("agent: WSS dial failed: %w", err)
	}
	a.conn = conn

	// Start read loop before waiting for welcome — the frame arrives
	// over that very loop. We listen on a one-shot channel for it.
	welcomeCh := make(chan WelcomeEvent, 1)
	go a.readLoop(welcomeCh)

	hbCtx, hbCancel := context.WithCancel(context.Background())
	a.hbCancel = hbCancel
	go a.heartbeatLoop(hbCtx)

	select {
	case w := <-welcomeCh:
		a.deviceID.Store(w.DeviceID)
		return nil
	case <-a.doneCh:
		return errors.New("agent: control channel closed before welcome")
	case <-ctx.Done():
		_ = a.Close()
		return ctx.Err()
	}
}

// SetState drives a state transition. Blocks until the gateway acks
// or returns an error. The ack/error path is the gateway's transition
// validator + presence_service Lua return; a network failure shows up
// here as `agent: control channel closed mid-transition`.
func (a *Agent) SetState(ctx context.Context, state AgentState, reason string) (int64, error) {
	if a.conn == nil {
		return 0, errors.New("agent: SetState called before Connect")
	}
	// Register before sending so we can't miss a fast ack.
	ackCh := make(chan ackOrErr, 1)
	a.pendingMu.Lock()
	a.pending[state] = append(a.pending[state], ackCh)
	a.pendingMu.Unlock()

	defer func() {
		a.pendingMu.Lock()
		// Best-effort removal — if the read loop already drained us,
		// the slice is shorter and this becomes a no-op.
		if list := a.pending[state]; len(list) > 0 {
			for i, ch := range list {
				if ch == ackCh {
					a.pending[state] = append(list[:i], list[i+1:]...)
					break
				}
			}
			if len(a.pending[state]) == 0 {
				delete(a.pending, state)
			}
		}
		a.pendingMu.Unlock()
	}()

	frame := map[string]any{"type": "set_state", "state": string(state)}
	if reason != "" {
		frame["reason"] = reason
	}
	if err := a.writeJSON(frame); err != nil {
		return 0, fmt.Errorf("agent: writeJSON failed: %w", err)
	}

	select {
	case r := <-ackCh:
		if r.err != nil {
			return 0, fmt.Errorf("%s: %s", r.err.Code, r.err.Message)
		}
		a.currentState.Store(state)
		return r.ack.TS, nil
	case <-a.doneCh:
		return 0, errors.New("agent: control channel closed mid-transition")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Close tears the channel down cleanly. Safe to call multiple times.
func (a *Agent) Close() error {
	var closeErr error
	a.closeOnce.Do(func() {
		if a.hbCancel != nil {
			a.hbCancel()
		}
		if a.conn != nil {
			a.connMu.Lock()
			closeErr = a.conn.WriteMessage(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "agent.Close"),
			)
			_ = a.conn.Close()
			a.connMu.Unlock()
		}
		close(a.doneCh)
		close(a.events)
	})
	return closeErr
}

// ─── Internal ───────────────────────────────────────────────────────

func (a *Agent) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.conn == nil {
		return errors.New("agent: not connected")
	}
	return a.conn.WriteMessage(websocket.TextMessage, b)
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(a.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Failure here is fine — the read loop will fire close
			// soon enough if the socket is actually gone.
			_ = a.writeJSON(map[string]string{"type": "heartbeat"})
		}
	}
}

func (a *Agent) readLoop(welcomeCh chan<- WelcomeEvent) {
	defer func() {
		// Synthesize a Closed event before tearing down so a consumer
		// blocked on Events() can react.
		a.sendEvent(AgentEvent{Closed: &ClosedEvent{Clean: false}})
		_ = a.Close()
	}()
	for {
		_, msg, err := a.conn.ReadMessage()
		if err != nil {
			return
		}
		var f struct {
			Type        string `json:"type"`
			State       string `json:"state"`
			AuxReason   string `json:"aux_reason"`
			TS          int64  `json:"ts"`
			DeviceID    string `json:"device_id"`
			Code        string `json:"code"`
			Msg         string `json:"msg"`
			Reason      string `json:"reason"`
			CallID      string `json:"call_id"`
			RingUntilMs int64  `json:"ring_until_ms"`
			MissedCount int64  `json:"missed_count"`
			ForcedRona  bool   `json:"forced_rona"`
		}
		if err := json.Unmarshal(msg, &f); err != nil {
			continue
		}
		switch f.Type {
		case "welcome":
			w := WelcomeEvent{DeviceID: f.DeviceID}
			// Welcome only fires once; non-blocking send so a
			// late-registered consumer can also pick it up via
			// Events() if they didn't catch the Connect path.
			select {
			case welcomeCh <- w:
			default:
			}
			a.sendEvent(AgentEvent{Welcome: &w})
		case "ack":
			ack := AckEvent{State: AgentState(f.State), TS: f.TS}
			a.routeAck(ack, nil)
		case "error":
			errEv := ErrorEvent{Code: f.Code, Message: f.Msg}
			// If we have any outstanding SetState calls, surface the
			// error to all of them — the gateway doesn't include the
			// rejected target in the error frame.
			a.routeAck(AckEvent{}, &errEv)
			a.sendEvent(AgentEvent{Err: &errEv})
		case "forced_logout":
			a.sendEvent(AgentEvent{ForcedLogout: &ForcedLogoutEvent{Reason: f.Reason}})
		case "state_changed":
			a.currentState.Store(AgentState(f.State))
			a.sendEvent(AgentEvent{StateChanged: &StateChangedEvent{
				State:     AgentState(f.State),
				AuxReason: f.AuxReason,
				TS:        f.TS,
			}})
		case "call_offered":
			offer := &CallOfferedEvent{CallID: f.CallID, RingUntilMs: f.RingUntilMs}
			a.pendingOffer.Store(offer)
			a.currentState.Store(StateRinging)
			a.sendEvent(AgentEvent{CallOffered: offer})
		case "call_offered_expired":
			a.pendingOffer.Store((*CallOfferedEvent)(nil))
			a.sendEvent(AgentEvent{CallOfferedExpired: &CallOfferedExpiredEvent{
				CallID:      f.CallID,
				MissedCount: f.MissedCount,
				ForcedRona:  f.ForcedRona,
			}})
		case "call_confirmed":
			confirmed := CallConfirmedEvent{CallID: f.CallID}
			a.routeConfirm(confirmed)
			a.sendEvent(AgentEvent{CallConfirmed: &confirmed})
		}
	}
}

func (a *Agent) routeConfirm(c CallConfirmedEvent) {
	a.confirmPendingMu.Lock()
	defer a.confirmPendingMu.Unlock()
	list := a.confirmPending[c.CallID]
	if len(list) == 0 {
		return
	}
	ch := list[0]
	a.confirmPending[c.CallID] = list[1:]
	if len(a.confirmPending[c.CallID]) == 0 {
		delete(a.confirmPending, c.CallID)
	}
	select {
	case ch <- confirmOrErr{confirmed: c}:
	default:
	}
}

func (a *Agent) routeAck(ack AckEvent, errEv *ErrorEvent) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	if errEv != nil {
		// Drain every pending channel — the next setState attempt
		// will need to re-register.
		for state, list := range a.pending {
			for _, ch := range list {
				select {
				case ch <- ackOrErr{err: errEv}:
				default:
				}
			}
			delete(a.pending, state)
		}
		return
	}
	if list := a.pending[ack.State]; len(list) > 0 {
		ch := list[0]
		a.pending[ack.State] = list[1:]
		if len(a.pending[ack.State]) == 0 {
			delete(a.pending, ack.State)
		}
		select {
		case ch <- ackOrErr{ack: ack}:
		default:
		}
	}
}

func (a *Agent) sendEvent(ev AgentEvent) {
	// Non-blocking — if the consumer fell behind, we drop the event.
	// Slice 2 may want to back-pressure on this, but Slice 1 prefers
	// progress over guaranteed delivery (the wallboard pubsub channel
	// is the source-of-truth path; SDK events are convenience).
	select {
	case a.events <- ev:
	default:
	}
}
