// Package voice — telephony modality over the MoQT substrate.
//
// Two primitives: Calls (control plane — originate, transfer, hangup over
// the BFF tRPC) and AudioBridge (data plane — bidirectional Opus / PCM /
// G.711 over MoQT with the voice/<sid>/{uplink,downlink} namespace
// convention enforced). Mirrors the TypeScript @clutchcall/sdk/voice and
// Python clutchcall.voice modules.
package voice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	clutchcall "github.com/clutchcall/go-sdk/pkg"
)

// ── error ───────────────────────────────────────────────────────────────

type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// ── voice client ────────────────────────────────────────────────────────

type Voice struct {
	BaseURL   string
	APIKey    string
	OrgID     string
	RelayHost string
	HTTP      *http.Client
}

func New(baseURL, apiKey, orgID string) (*Voice, error) {
	if baseURL == "" { return nil, &Error{"Voice: BaseURL required"} }
	if apiKey  == "" { return nil, &Error{"Voice: APIKey required"} }
	if orgID   == "" { return nil, &Error{"Voice: OrgID required"} }
	return &Voice{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		OrgID:     orgID,
		RelayHost: "relay.clutchcall.dev",
		HTTP:      http.DefaultClient,
	}, nil
}

func (v *Voice) Calls() *Calls               { return &Calls{v: v} }
func (v *Voice) AudioBridge() *AudioBridgeFactory { return &AudioBridgeFactory{v: v} }
func (v *Voice) Agents() *Agents             { return &Agents{v: v} }

// call is the internal tRPC HTTP shape.
func (v *Voice) call(path string, payload any, mutation bool, out any) error {
	url_ := v.BaseURL + "/api/trpc/" + path
	var req *http.Request
	var err error
	if mutation {
		body, _ := json.Marshal(payload)
		req, err = http.NewRequest(http.MethodPost, url_, bytes.NewReader(body))
	} else {
		j, _ := json.Marshal(payload)
		q := url.Values{}
		q.Set("input", string(j))
		req, err = http.NewRequest(http.MethodGet, url_+"?"+q.Encode(), nil)
	}
	if err != nil { return err }
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+v.APIKey)
	resp, err := v.HTTP.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		s := string(b); if len(s) > 200 { s = s[:200] }
		return fmt.Errorf("tRPC %s %d: %s", path, resp.StatusCode, s)
	}
	var env struct {
		Result *struct{ Data json.RawMessage `json:"data"` } `json:"result"`
		Error  *struct{ Message string `json:"message"` }    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("tRPC %s decode: %w", path, err)
	}
	if env.Error != nil { return errors.New(env.Error.Message) }
	if env.Result == nil { return fmt.Errorf("tRPC %s: empty result", path) }
	return json.Unmarshal(env.Result.Data, out)
}

// ── calls ───────────────────────────────────────────────────────────────

type CallData struct {
	Sid       string `json:"sid"`
	Status    string `json:"status"`
	To        string `json:"to"`
	From      string `json:"from"`
	StartedAt string `json:"startedAt"`
	TrunkID   string `json:"trunkId,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

type OriginateArgs struct {
	To, From, TrunkID, Agent string
	RingTimeoutSec           int
}

type Calls struct{ v *Voice }

func (c *Calls) Originate(args OriginateArgs) (*Call, error) {
	if args.RingTimeoutSec == 0 { args.RingTimeoutSec = 30 }
	var row CallData
	err := c.v.call("voice.calls.originate", map[string]any{
		"orgId":          c.v.OrgID,
		"to":             args.To,
		"from":           args.From,
		"trunkId":        args.TrunkID,
		"agent":          args.Agent,
		"ringTimeoutSec": args.RingTimeoutSec,
	}, true, &row)
	if err != nil { return nil, err }
	return &Call{v: c.v, CallData: row}, nil
}

func (c *Calls) Get(sid string) (*Call, error) {
	var row CallData
	err := c.v.call("voice.calls.get",
		map[string]any{"orgId": c.v.OrgID, "sid": sid}, false, &row)
	if err != nil { return nil, err }
	return &Call{v: c.v, CallData: row}, nil
}

type Call struct {
	CallData
	v *Voice
}

// Transfer to a PSTN number (set To) OR re-attach to a different agent
// (set Agent). Exactly one of the two.
type TransferArgs struct{ To, Agent string }

func (c *Call) Transfer(args TransferArgs) error {
	if (args.To == "") == (args.Agent == "") {
		return &Error{"Transfer: pass exactly one of To=<E.164> or Agent=<id>"}
	}
	var ok struct{ OK bool }
	return c.v.call("voice.calls.transfer",
		map[string]any{"orgId": c.v.OrgID, "sid": c.Sid, "to": args.To, "agent": args.Agent},
		true, &ok)
}

func (c *Call) Hangup() error {
	var ok struct{ OK bool }
	return c.v.call("voice.calls.hangup",
		map[string]any{"orgId": c.v.OrgID, "sid": c.Sid}, true, &ok)
}

// ── audio bridge ────────────────────────────────────────────────────────

type Codec string

const (
	CodecOpus     Codec = "opus"
	CodecPCM16    Codec = "pcm16"
	CodecG711ULaw Codec = "g711_ulaw"
	CodecG711ALaw Codec = "g711_alaw"
)

type AudioBridgeOpts struct {
	Codec      Codec
	SampleRate uint32
	Channels   uint8
	FrameMs    uint16
	OnUplink   func(frame []byte, timestampUs uint64)
}

type AudioBridge struct {
	client *clutchcall.MoqtClient
	pub    *clutchcall.AudioPublication
	sub    *clutchcall.AudioSubscription
	CallSid string
}

// PublishDownlink pushes one encoded audio frame to the caller.
func (b *AudioBridge) PublishDownlink(frame []byte) {
	b.pub.Write(uint64(time.Now().UnixNano()/1000), frame)
}

func (b *AudioBridge) Close() {
	if b.pub != nil    { b.pub.Close() }
	if b.sub != nil    { b.sub.Close() }
	if b.client != nil { b.client.Close() }
}

type AudioBridgeFactory struct{ v *Voice }

func (f *AudioBridgeFactory) Attach(callSid string, opts AudioBridgeOpts) (*AudioBridge, error) {
	if callSid == "" { return nil, &Error{"Attach: callSid required"} }
	if opts.OnUplink == nil { return nil, &Error{"Attach: OnUplink required"} }
	if opts.Codec      == "" { opts.Codec = CodecOpus }
	if opts.SampleRate == 0  { opts.SampleRate = 48000 }
	if opts.Channels   == 0  { opts.Channels = 1 }
	if opts.FrameMs    == 0  { opts.FrameMs = 20 }

	url_ := fmt.Sprintf("moq://%s/voice/%s", f.v.RelayHost, url.PathEscape(callSid))
	client, err := clutchcall.ConnectMoqt(url_, f.v.APIKey, func(int) {})
	if err != nil { return nil, err }

	// SubscribeAudio uses (ts, frame) ordering; OnUplink takes (frame, ts) to
	// match the pattern of every other modality callback in this SDK.
	sub, err := client.SubscribeAudio("voice/"+callSid+"/uplink", "audio",
		func(ts uint64, frame []byte) { opts.OnUplink(frame, ts) })
	if err != nil { client.Close(); return nil, err }

	pub, err := client.PublishAudio("voice/"+callSid+"/downlink", "audio",
		"voice/"+string(opts.Codec), opts.SampleRate, opts.Channels, opts.FrameMs)
	if err != nil { sub.Close(); client.Close(); return nil, err }

	return &AudioBridge{client: client, pub: pub, sub: sub, CallSid: callSid}, nil
}

// ── agents ──────────────────────────────────────────────────────────────

type Agents struct{ v *Voice }

func (a *Agents) Attach(callSid, agent string) error {
	if callSid == "" { return &Error{"Agents.Attach: callSid required"} }
	if agent   == "" { return &Error{"Agents.Attach: agent required"} }
	var ok struct{ OK bool }
	return a.v.call("voice.agents.attach",
		map[string]any{"orgId": a.v.OrgID, "sid": callSid, "agent": agent}, true, &ok)
}
