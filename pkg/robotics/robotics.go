// Package robotics — typed pub/sub for a robot fleet over the MoQT substrate.
//
// Bidirectional teleop convention baked in: telemetry on robot/<id>,
// commands on robot/<id>/ctl. Wire format adds a type-name prefix so a
// Python subscriber and a Go publisher pick the right deserializer with
// no out-of-band agreement.
//
// Mirrors @clutchcall/sdk/robotics and clutchcall.robotics.
package robotics

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"time"

	clutchcall "github.com/clutchcall/go-sdk/pkg"
)

// ── wire format ─────────────────────────────────────────────────────────

const (
	HeaderBytes = 2
	MaxTypeName = 0xFFFF
)

// EncodeFrame writes [u16 BE type_name_len][type_name][payload].
func EncodeFrame(typeName string, payload []byte) ([]byte, error) {
	if typeName == "" {
		return nil, errors.New("robotics: typeName required")
	}
	if len(typeName) > MaxTypeName {
		return nil, fmt.Errorf("robotics: typeName > 65535 bytes (%d)", len(typeName))
	}
	out := make([]byte, HeaderBytes+len(typeName)+len(payload))
	binary.BigEndian.PutUint16(out[0:2], uint16(len(typeName)))
	copy(out[HeaderBytes:], typeName)
	copy(out[HeaderBytes+len(typeName):], payload)
	return out, nil
}

// DecodedFrame is the result of DecodeFrame.
type DecodedFrame struct {
	TypeName string
	Payload  []byte
}

// DecodeFrame parses [u16 BE type_name_len][type_name][payload].
func DecodeFrame(buf []byte) (*DecodedFrame, error) {
	if len(buf) < HeaderBytes {
		return nil, errors.New("robotics: frame too short")
	}
	n := int(binary.BigEndian.Uint16(buf[0:2]))
	end := HeaderBytes + n
	if len(buf) < end {
		return nil, fmt.Errorf("robotics: truncated frame (typeNameLen=%d, available=%d)", n, len(buf)-HeaderBytes)
	}
	return &DecodedFrame{
		TypeName: string(buf[HeaderBytes:end]),
		Payload:  append([]byte(nil), buf[end:]...),
	}, nil
}

// ── QoS ─────────────────────────────────────────────────────────────────

type Reliability string

const (
	BestEffort Reliability = "best_effort"
	Reliable   Reliability = "reliable"
)

type Durability string

const (
	Volatile       Durability = "volatile"
	TransientLocal Durability = "transient_local"
)

type QoSProfile struct {
	Reliability Reliability
	Durability  Durability
	Depth       int
}

func (q QoSProfile) capability() string {
	rel, dur := q.Reliability, q.Durability
	if rel == "" {
		rel = BestEffort
	}
	if dur == "" {
		dur = Volatile
	}
	if dur == TransientLocal {
		if rel == Reliable {
			return "ros.tl_reliable"
		}
		return "ros.tl_be"
	}
	if rel == Reliable {
		return "ros.reliable"
	}
	return "ros.best_effort"
}

func (q QoSProfile) defaultPriority() uint8 {
	rel := q.Reliability
	if rel == "" {
		rel = BestEffort
	}
	if rel == Reliable {
		return 50
	}
	return 100
}

// ── handles ─────────────────────────────────────────────────────────────

type Publication struct {
	track            *clutchcall.FramePublication
	typeName         string
	defaultPriority  uint8
}

// Write pushes one typed message. Payload is the raw CDR bytes (or
// whatever matches typeName). Pass priority=255 to mean "use default".
func (p *Publication) Write(payload []byte, priority uint8) error {
	frame, err := EncodeFrame(p.typeName, payload)
	if err != nil {
		return err
	}
	if priority == 255 {
		priority = p.defaultPriority
	}
	p.track.Write(uint64(time.Now().UnixNano()/1000), frame, priority)
	return nil
}

func (p *Publication) Close() { p.track.Close() }

type Subscription struct {
	sub  *clutchcall.FrameSubscription
	NS   string
	Name string
}

func (s *Subscription) Close() { s.sub.Close() }

// ── client ──────────────────────────────────────────────────────────────

// Robotics client — one per (tenant, robot). The MoQT session is opened
// lazily on the first publish/subscribe and reused.
type Robotics struct {
	RelayHost string
	Token     string
	RobotID   string
	OnState   func(int)

	client *clutchcall.MoqtClient
}

func New(token, robotID string) (*Robotics, error) {
	if token == "" {
		return nil, errors.New("Robotics: Token required")
	}
	if robotID == "" {
		return nil, errors.New("Robotics: RobotID required")
	}
	return &Robotics{
		RelayHost: "relay.clutchcall.dev",
		Token:     token,
		RobotID:   robotID,
	}, nil
}

func (r *Robotics) TelemetryNS() string { return "robot/" + r.RobotID }
func (r *Robotics) CommandNS() string   { return "robot/" + r.RobotID + "/ctl" }

func (r *Robotics) ensure() (*clutchcall.MoqtClient, error) {
	if r.client != nil {
		return r.client, nil
	}
	host := r.RelayHost
	if host == "" {
		host = "relay.clutchcall.dev"
	}
	u := fmt.Sprintf("moq://%s/robotics/%s", host, url.PathEscape(r.RobotID))
	on := r.OnState
	if on == nil {
		on = func(int) {}
	}
	c, err := clutchcall.ConnectMoqt(u, r.Token, on)
	if err != nil {
		return nil, err
	}
	r.client = c
	return c, nil
}

func (r *Robotics) PublishTelemetry(topic, typeName string, qos QoSProfile) (*Publication, error) {
	return r.publish(r.TelemetryNS(), topic, typeName, qos)
}

func (r *Robotics) SubscribeTelemetry(topic, typeName string, onMessage func(typeName string, payload []byte)) (*Subscription, error) {
	return r.subscribe(r.TelemetryNS(), topic, onMessage)
}

func (r *Robotics) PublishCommand(topic, typeName string, qos QoSProfile) (*Publication, error) {
	return r.publish(r.CommandNS(), topic, typeName, qos)
}

func (r *Robotics) SubscribeCommand(topic, typeName string, onMessage func(typeName string, payload []byte)) (*Subscription, error) {
	return r.subscribe(r.CommandNS(), topic, onMessage)
}

func (r *Robotics) Close() {
	if r.client != nil {
		r.client.Close()
		r.client = nil
	}
}

func (r *Robotics) publish(ns, name, typeName string, qos QoSProfile) (*Publication, error) {
	c, err := r.ensure()
	if err != nil {
		return nil, err
	}
	t, err := c.PublishFrame(ns, name, qos.capability(),
		"ros2/cdr;type="+typeName, 0)
	if err != nil {
		return nil, err
	}
	return &Publication{track: t, typeName: typeName, defaultPriority: qos.defaultPriority()}, nil
}

func (r *Robotics) subscribe(ns, name string, onMessage func(string, []byte)) (*Subscription, error) {
	c, err := r.ensure()
	if err != nil {
		return nil, err
	}
	sub, err := c.SubscribeFrame(ns, name, func(_ uint64, _ uint8, data []byte) {
		f, err := DecodeFrame(data)
		if err != nil {
			return // silently drop malformed; relay logs the reject
		}
		onMessage(f.TypeName, f.Payload)
	})
	if err != nil {
		return nil, err
	}
	return &Subscription{sub: sub, NS: ns, Name: name}, nil
}
