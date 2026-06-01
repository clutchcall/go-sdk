// Agent unit tests — drive the SDK against an httptest WSS server
// that impersonates the gateway's control channel. Pure Go; no Redis,
// no real engine. Validates:
//
//   * upgrade with Authorization: Bearer <token>
//   * welcome → CurrentState is offline until SetState succeeds
//   * SetState round-trip ack
//   * Error frame surfaces as a SetState error
//   * Heartbeat frames are emitted on the configured cadence
//   * Close cleans up the connection

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeGateway is a minimal /agent/control impersonator. It records the
// authorization header it saw + every frame the SDK sent, and replies
// with whatever the test plants in `replies`.
type fakeGateway struct {
	mu sync.Mutex

	authSeen     string
	framesSeen   []map[string]any
	heartbeatCnt atomic.Int32

	// replies maps an incoming frame's `type` to the bytes the server
	// sends back. The test plants these before Connect.
	replies map[string][]byte

	server *httptest.Server
}

func newFakeGateway(t *testing.T, replies map[string][]byte) *fakeGateway {
	t.Helper()
	fg := &fakeGateway{replies: replies}
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	fg.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fg.mu.Lock()
		fg.authSeen = r.Header.Get("Authorization")
		fg.mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("fake gateway upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		// Send welcome immediately so Connect resolves.
		if msg, ok := fg.replies["welcome"]; ok {
			_ = conn.WriteMessage(websocket.TextMessage, msg)
		}

		// Read loop: echo whatever the SDK sends to fg.framesSeen and
		// plant the configured reply.
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var f map[string]any
			if err := json.Unmarshal(payload, &f); err != nil {
				continue
			}
			fg.mu.Lock()
			fg.framesSeen = append(fg.framesSeen, f)
			fg.mu.Unlock()

			t, _ := f["type"].(string)
			if t == "heartbeat" {
				fg.heartbeatCnt.Add(1)
				continue
			}
			if msg, ok := fg.replies[t]; ok {
				_ = conn.WriteMessage(websocket.TextMessage, msg)
			}
		}
	}))
	t.Cleanup(fg.server.Close)
	return fg
}

func (fg *fakeGateway) wssURL() string {
	return "ws" + strings.TrimPrefix(fg.server.URL, "http") + "/agent/control"
}

func (fg *fakeGateway) frameCount() int {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return len(fg.framesSeen)
}

func TestAgent_ConnectAndWelcome(t *testing.T) {
	fg := newFakeGateway(t, map[string][]byte{
		"welcome": []byte(`{"type":"welcome","device_id":"dev-42"}`),
	})

	a := NewAgent(AgentOptions{
		WssURL:            fg.wssURL(),
		Token:             "test-token",
		HeartbeatInterval: 100 * time.Millisecond,
		DialTimeout:       2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer a.Close()

	if a.DeviceID() != "dev-42" {
		t.Errorf("DeviceID = %q, want dev-42", a.DeviceID())
	}
	if a.CurrentState() != StateOffline {
		t.Errorf("CurrentState = %q before SetState, want offline", a.CurrentState())
	}

	// The Authorization header must have made it through verbatim.
	fg.mu.Lock()
	auth := fg.authSeen
	fg.mu.Unlock()
	if auth != "Bearer test-token" {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer test-token")
	}
}

func TestAgent_SetStateAck(t *testing.T) {
	fg := newFakeGateway(t, map[string][]byte{
		"welcome":   []byte(`{"type":"welcome","device_id":"dev-1"}`),
		"set_state": []byte(`{"type":"ack","state":"ready","ts":123456}`),
	})

	a := NewAgent(AgentOptions{
		WssURL:            fg.wssURL(),
		Token:             "t",
		HeartbeatInterval: 1 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer a.Close()

	ts, err := a.SetState(ctx, StateReady, "")
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if ts != 123456 {
		t.Errorf("ts = %d, want 123456", ts)
	}
	if a.CurrentState() != StateReady {
		t.Errorf("CurrentState = %q, want ready", a.CurrentState())
	}
}

func TestAgent_SetStateError(t *testing.T) {
	fg := newFakeGateway(t, map[string][]byte{
		"welcome":   []byte(`{"type":"welcome","device_id":"dev-1"}`),
		"set_state": []byte(`{"type":"error","code":"illegal_transition","msg":"aux -> on_call rejected"}`),
	})

	a := NewAgent(AgentOptions{WssURL: fg.wssURL(), Token: "t"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer a.Close()

	_, err := a.SetState(ctx, StateOnCall, "")
	if err == nil {
		t.Fatalf("SetState succeeded, want error")
	}
	if !strings.Contains(err.Error(), "illegal_transition") {
		t.Errorf("error = %q, want it to mention illegal_transition", err)
	}
	// On error the local state stays put.
	if a.CurrentState() != StateOffline {
		t.Errorf("CurrentState after rejected transition = %q, want offline", a.CurrentState())
	}
}

func TestAgent_HeartbeatEmits(t *testing.T) {
	fg := newFakeGateway(t, map[string][]byte{
		"welcome": []byte(`{"type":"welcome","device_id":"dev-1"}`),
	})
	a := NewAgent(AgentOptions{
		WssURL:            fg.wssURL(),
		Token:             "t",
		HeartbeatInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := a.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer a.Close()

	// Wait until at least 3 heartbeats have landed. 50ms cadence + 1s
	// budget gives plenty of headroom on CI.
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fg.heartbeatCnt.Load() >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := fg.heartbeatCnt.Load(); got < 3 {
		t.Errorf("heartbeats received = %d, want at least 3", got)
	}
}

func TestAgent_CloseIsIdempotent(t *testing.T) {
	fg := newFakeGateway(t, map[string][]byte{
		"welcome": []byte(`{"type":"welcome","device_id":"dev-1"}`),
	})
	a := NewAgent(AgentOptions{WssURL: fg.wssURL(), Token: "t"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Multiple Close calls must not panic, must not double-close
	// channels.
	_ = a.Close()
	_ = a.Close()
	_ = a.Close()
}

func TestAgent_ConnectFailsOnUnreachable(t *testing.T) {
	a := NewAgent(AgentOptions{
		WssURL:      "ws://127.0.0.1:1/agent/control", // port 1 — guaranteed connect refused
		Token:       "t",
		DialTimeout: 200 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := a.Connect(ctx); err == nil {
		t.Errorf("Connect succeeded on unreachable port; want error")
		_ = a.Close()
	}
}
