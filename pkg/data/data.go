// Package data — MQTT-style typed pub/sub over the MoQT substrate.
//
// Hierarchical topics with `+` / `#` wildcards (top-level segment must
// be concrete). The frame header carries the full topic + the publisher's
// client id so subscribers MQTT-filter and attribute without out-of-band
// lookup.
//
// Mirrors @clutchcall/sdk/data and clutchcall.data.
package data

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	clutchcall "github.com/clutchcall/go-sdk/pkg"
)

// ── wire ────────────────────────────────────────────────────────────────

const (
	FromLenBytes  = 1
	TopicLenBytes = 1
	MaxFromLen    = 0xFF
	MaxTopicLen   = 0xFF
)

// EncodeDataFrame: [u8 from_len][from][u8 topic_len][topic][payload].
func EncodeDataFrame(fromClientID, topic string, payload []byte) ([]byte, error) {
	if len(fromClientID) > MaxFromLen {
		return nil, fmt.Errorf("data: fromClientID > 255 bytes (%d)", len(fromClientID))
	}
	if len(topic) > MaxTopicLen {
		return nil, fmt.Errorf("data: topic > 255 bytes (%d)", len(topic))
	}
	out := make([]byte, 1+len(fromClientID)+1+len(topic)+len(payload))
	o := 0
	out[o] = byte(len(fromClientID))
	o++
	copy(out[o:], fromClientID)
	o += len(fromClientID)
	out[o] = byte(len(topic))
	o++
	copy(out[o:], topic)
	o += len(topic)
	copy(out[o:], payload)
	return out, nil
}

type DecodedDataFrame struct {
	FromClientID string
	Topic        string
	Payload      []byte
}

func DecodeDataFrame(buf []byte) (*DecodedDataFrame, error) {
	if len(buf) < 1 {
		return nil, errors.New("data: frame too short")
	}
	fromLen := int(buf[0])
	pos := 1
	if len(buf) < pos+fromLen+1 {
		return nil, errors.New("data: truncated frame (from + topic_len)")
	}
	from := string(buf[pos : pos+fromLen])
	pos += fromLen
	topicLen := int(buf[pos])
	pos++
	if len(buf) < pos+topicLen {
		return nil, errors.New("data: truncated frame (topic)")
	}
	topic := string(buf[pos : pos+topicLen])
	pos += topicLen
	return &DecodedDataFrame{
		FromClientID: from,
		Topic:        topic,
		Payload:      append([]byte(nil), buf[pos:]...),
	}, nil
}

// TopicMatches: MQTT-style match. `+` matches one segment; `#` matches
// the rest (must be the last segment).
func TopicMatches(topic, topicFilter string) bool {
	if topic == topicFilter {
		return true
	}
	t := strings.Split(topic, "/")
	f := strings.Split(topicFilter, "/")
	for i, fp := range f {
		if fp == "#" {
			return i == len(f)-1
		}
		if i >= len(t) {
			return false
		}
		if fp == "+" {
			continue
		}
		if fp != t[i] {
			return false
		}
	}
	return len(t) == len(f)
}

// TopLevelSegment returns the concrete top-level path segment; errors if
// it's a wildcard.
func TopLevelSegment(filterOrTopic string) (string, error) {
	head := strings.SplitN(filterOrTopic, "/", 2)[0]
	if head == "+" || head == "#" {
		return "", fmt.Errorf("data: top-level wildcard not supported (%q)", filterOrTopic)
	}
	if head == "" {
		return "", errors.New("data: empty topic / filter")
	}
	return head, nil
}

// ── message ─────────────────────────────────────────────────────────────

type Message struct {
	Topic        string
	FromClientID string
	Payload      []byte
	Retained     bool
}

type Subscription struct {
	sub         *clutchcall.FrameSubscription
	NS          string
	TopicFilter string
}

func (s *Subscription) Close() { s.sub.Close() }

// ── client ──────────────────────────────────────────────────────────────

type Data struct {
	RelayHost string
	Token     string
	ClientID  string
	OnState   func(int)

	client *clutchcall.MoqtClient
	mu     sync.Mutex
	pubs   map[string]*clutchcall.FramePublication
}

func New(token, clientID string) (*Data, error) {
	if token == "" {
		return nil, errors.New("Data: Token required")
	}
	if clientID == "" {
		return nil, errors.New("Data: ClientID required")
	}
	return &Data{
		RelayHost: "relay.clutchcall.dev",
		Token:     token,
		ClientID:  clientID,
		pubs:      map[string]*clutchcall.FramePublication{},
	}, nil
}

func (d *Data) ensure() (*clutchcall.MoqtClient, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		return d.client, nil
	}
	host := d.RelayHost
	if host == "" {
		host = "relay.clutchcall.dev"
	}
	u := fmt.Sprintf("moq://%s/data/%s", host, url.PathEscape(d.ClientID))
	on := d.OnState
	if on == nil {
		on = func(int) {}
	}
	c, err := clutchcall.ConnectMoqt(u, d.Token, on)
	if err != nil {
		return nil, err
	}
	d.client = c
	return c, nil
}

func (d *Data) getPub(top string) (*clutchcall.FramePublication, error) {
	d.mu.Lock()
	if p, ok := d.pubs[top]; ok {
		d.mu.Unlock()
		return p, nil
	}
	d.mu.Unlock()
	c, err := d.ensure()
	if err != nil {
		return nil, err
	}
	p, err := c.PublishFrame("data/"+top, "msg",
		"data.pubsub", "data;top="+top, 100)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.pubs[top] = p
	d.mu.Unlock()
	return p, nil
}

// Publish one message. retained=true tells the relay to cache the latest
// payload for this topic and re-deliver on every new matching subscriber.
func (d *Data) Publish(topic string, payload []byte, reliable, retained bool) error {
	top, err := TopLevelSegment(topic)
	if err != nil {
		return err
	}
	p, err := d.getPub(top)
	if err != nil {
		return err
	}
	frame, err := EncodeDataFrame(d.ClientID, topic, payload)
	if err != nil {
		return err
	}
	priority := uint8(100)
	if reliable || retained {
		priority = 30
	}
	p.Write(uint64(time.Now().UnixNano()/1000), frame, priority)
	return nil
}

// Subscribe with an MQTT-style filter. The SDK opens one MoQT
// subscription per top-level filter segment and filters the rest
// client-side.
func (d *Data) Subscribe(topicFilter string, onMessage func(Message)) (*Subscription, error) {
	top, err := TopLevelSegment(topicFilter)
	if err != nil {
		return nil, err
	}
	c, err := d.ensure()
	if err != nil {
		return nil, err
	}
	ns := "data/" + top
	s, err := c.SubscribeFrame(ns, "msg", func(_ uint64, prio uint8, raw []byte) {
		f, err := DecodeDataFrame(raw)
		if err != nil {
			return
		}
		if !TopicMatches(f.Topic, topicFilter) {
			return
		}
		onMessage(Message{
			Topic:        f.Topic,
			FromClientID: f.FromClientID,
			Payload:      f.Payload,
			Retained:     prio <= 30,
		})
	})
	if err != nil {
		return nil, err
	}
	return &Subscription{sub: s, NS: ns, TopicFilter: topicFilter}, nil
}

func (d *Data) Close() {
	d.mu.Lock()
	for _, p := range d.pubs {
		p.Close()
	}
	d.pubs = map[string]*clutchcall.FramePublication{}
	c := d.client
	d.client = nil
	d.mu.Unlock()
	if c != nil {
		c.Close()
	}
}
