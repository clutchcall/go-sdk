// Package games — multiplayer rooms over the MoQT substrate.
//
// Three channels per room: state (server → all), input (player → server),
// event (any → any). Namespaces baked in; input + event frames carry a
// 1-byte from-header so the server's single subscribe callback can sort
// frames by player.
//
// Mirrors @clutchcall/sdk/games and clutchcall.games.
package games

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	clutchcall "github.com/clutchcall/go-sdk/pkg"
)

// ── wire ────────────────────────────────────────────────────────────────

const (
	FromHeaderBytes = 1
	MaxFromLen      = 0xFF
)

func EncodeWithFrom(fromPlayerID string, payload []byte) ([]byte, error) {
	if len(fromPlayerID) > MaxFromLen {
		return nil, fmt.Errorf("games: fromPlayerID > 255 bytes (%d)", len(fromPlayerID))
	}
	out := make([]byte, 1+len(fromPlayerID)+len(payload))
	out[0] = byte(len(fromPlayerID))
	copy(out[1:], fromPlayerID)
	copy(out[1+len(fromPlayerID):], payload)
	return out, nil
}

type DecodedWithFrom struct {
	FromPlayerID string
	Payload      []byte
}

func DecodeWithFrom(buf []byte) (*DecodedWithFrom, error) {
	if len(buf) < 1 {
		return nil, errors.New("games: frame too short")
	}
	n := int(buf[0])
	if len(buf) < 1+n {
		return nil, fmt.Errorf("games: truncated frame (fromLen=%d, available=%d)", n, len(buf)-1)
	}
	return &DecodedWithFrom{
		FromPlayerID: string(buf[1 : 1+n]),
		Payload:      append([]byte(nil), buf[1+n:]...),
	}, nil
}

// ── handles ─────────────────────────────────────────────────────────────

// StatePublisher writes raw state bytes (no from-header) — server only.
type StatePublisher struct{ track *clutchcall.FramePublication }

func (p *StatePublisher) Write(stateBytes []byte) {
	p.track.Write(uint64(time.Now().UnixNano()/1000), stateBytes, 100)
}
func (p *StatePublisher) Close() { p.track.Close() }

// FromPublisher prefixes every frame with the caller's player id.
type FromPublisher struct {
	track            *clutchcall.FramePublication
	fromPlayerID     string
	defaultPriority  uint8
}

func (p *FromPublisher) Write(payload []byte) error {
	frame, err := EncodeWithFrom(p.fromPlayerID, payload)
	if err != nil {
		return err
	}
	p.track.Write(uint64(time.Now().UnixNano()/1000), frame, p.defaultPriority)
	return nil
}
func (p *FromPublisher) Close() { p.track.Close() }

type Subscription struct{ sub *clutchcall.FrameSubscription }

func (s *Subscription) Close() { s.sub.Close() }

// ── client ──────────────────────────────────────────────────────────────

type Games struct {
	RelayHost string
	Token     string
	RoomID    string
	PlayerID  string // empty = server / authority
	OnState   func(int)

	client *clutchcall.MoqtClient
}

func New(token, roomID, playerID string) (*Games, error) {
	if token == "" {
		return nil, errors.New("Games: Token required")
	}
	if roomID == "" {
		return nil, errors.New("Games: RoomID required")
	}
	return &Games{
		RelayHost: "relay.clutchcall.dev",
		Token:     token,
		RoomID:    roomID,
		PlayerID:  playerID,
	}, nil
}

func (g *Games) StateNS() string { return "game/" + g.RoomID + "/state" }
func (g *Games) InputNS() string { return "game/" + g.RoomID + "/input" }
func (g *Games) EventNS(channel string) string {
	return "game/" + g.RoomID + "/event/" + url.PathEscape(channel)
}

func (g *Games) ensure() (*clutchcall.MoqtClient, error) {
	if g.client != nil {
		return g.client, nil
	}
	host := g.RelayHost
	if host == "" {
		host = "relay.clutchcall.dev"
	}
	pid := "/_authority"
	if g.PlayerID != "" {
		pid = "/" + url.PathEscape(g.PlayerID)
	}
	u := fmt.Sprintf("moq://%s/games/%s%s", host, url.PathEscape(g.RoomID), pid)
	on := g.OnState
	if on == nil {
		on = func(int) {}
	}
	c, err := clutchcall.ConnectMoqt(u, g.Token, on)
	if err != nil {
		return nil, err
	}
	g.client = c
	return c, nil
}

// PublishState is server-only (no from-header).
func (g *Games) PublishState(tickHz int) (*StatePublisher, error) {
	c, err := g.ensure()
	if err != nil {
		return nil, err
	}
	tag := "game/state"
	if tickHz > 0 {
		tag = fmt.Sprintf("game/state;tickHz=%d", tickHz)
	}
	t, err := c.PublishFrame(g.StateNS(), "tick", "game.state", tag, 100)
	if err != nil {
		return nil, err
	}
	return &StatePublisher{track: t}, nil
}

func (g *Games) SubscribeState(onState func(stateBytes []byte)) (*Subscription, error) {
	c, err := g.ensure()
	if err != nil {
		return nil, err
	}
	s, err := c.SubscribeFrame(g.StateNS(), "tick", func(_ uint64, _ uint8, data []byte) {
		onState(append([]byte(nil), data...))
	})
	if err != nil {
		return nil, err
	}
	return &Subscription{sub: s}, nil
}

// PublishInput requires a PlayerID — the SDK rejects server-side calls.
func (g *Games) PublishInput() (*FromPublisher, error) {
	if g.PlayerID == "" {
		return nil, errors.New("Games.PublishInput: PlayerID required — only players can publish input")
	}
	c, err := g.ensure()
	if err != nil {
		return nil, err
	}
	t, err := c.PublishFrame(g.InputNS(), "frame", "game.input", "game/input", 100)
	if err != nil {
		return nil, err
	}
	return &FromPublisher{track: t, fromPlayerID: g.PlayerID, defaultPriority: 100}, nil
}

// SubscribeInputs fires for every player's input frame.
func (g *Games) SubscribeInputs(onInput func(fromPlayerID string, inputBytes []byte)) (*Subscription, error) {
	c, err := g.ensure()
	if err != nil {
		return nil, err
	}
	s, err := c.SubscribeFrame(g.InputNS(), "frame", func(_ uint64, _ uint8, data []byte) {
		d, err := DecodeWithFrom(data)
		if err != nil {
			return
		}
		onInput(d.FromPlayerID, d.Payload)
	})
	if err != nil {
		return nil, err
	}
	return &Subscription{sub: s}, nil
}

func (g *Games) PublishEvent(channel string) (*FromPublisher, error) {
	if channel == "" {
		return nil, errors.New("Games.PublishEvent: channel required")
	}
	from := g.PlayerID
	if from == "" {
		from = "_authority"
	}
	c, err := g.ensure()
	if err != nil {
		return nil, err
	}
	t, err := c.PublishFrame(g.EventNS(channel), "msg",
		"game.event", "game/event;channel="+channel, 50)
	if err != nil {
		return nil, err
	}
	return &FromPublisher{track: t, fromPlayerID: from, defaultPriority: 50}, nil
}

func (g *Games) SubscribeEvents(channel string, onEvent func(fromPlayerID string, eventBytes []byte)) (*Subscription, error) {
	if channel == "" {
		return nil, errors.New("Games.SubscribeEvents: channel required")
	}
	c, err := g.ensure()
	if err != nil {
		return nil, err
	}
	ns := g.EventNS(channel)
	s, err := c.SubscribeFrame(ns, "msg", func(_ uint64, _ uint8, data []byte) {
		d, err := DecodeWithFrom(data)
		if err != nil {
			return
		}
		onEvent(d.FromPlayerID, d.Payload)
	})
	if err != nil {
		return nil, err
	}
	return &Subscription{sub: s}, nil
}

func (g *Games) Close() {
	if g.client != nil {
		g.client.Close()
		g.client = nil
	}
}
