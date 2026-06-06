// Package streams — broadcast modality on top of the MoQT substrate.
//
// One client for the control plane (live inputs, signing keys, mint
// playback token), one helper for the data plane (BroadcastViewer wraps
// the MoQT client so callers don't have to know about relay path
// conventions). Mirrors the TypeScript @clutchcall/sdk/streams and the
// Python clutchcall.streams modules so docs stay one document.
//
// See https://github.com/clutchcall/skills/tree/master/skills/clutchcall-streams
// for the worked walkthrough.
package streams

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	clutchcall "github.com/clutchcall/go-sdk/pkg"
)

// ── client ──────────────────────────────────────────────────────────────

// Streams is the control-plane client. Stateless: every call is one tRPC
// mutation or query over HTTPS. The persistent QUIC connection happens
// inside BroadcastViewer / BroadcastPublisher.
type Streams struct {
	BaseURL string // BFF origin, e.g. https://app.clutchcall.dev
	APIKey  string // bearer token
	OrgID   string // tenant; required for tenant-scoped routes
	HTTP    *http.Client
}

// New is the shorthand constructor. Validates the required fields.
func New(baseURL, apiKey, orgID string) (*Streams, error) {
	if baseURL == "" {
		return nil, errors.New("streams: baseURL required")
	}
	if apiKey == "" {
		return nil, errors.New("streams: apiKey required")
	}
	return &Streams{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		OrgID:   orgID,
		HTTP:    http.DefaultClient,
	}, nil
}

// LiveInputs scopes the live-input procs to this client.
func (s *Streams) LiveInputs() *LiveInputs { return &LiveInputs{s: s} }

// SigningKeys scopes the signing-key procs to this client.
func (s *Streams) SigningKeys() *SigningKeys { return &SigningKeys{s: s} }

// call is the internal tRPC HTTP shape: GET for queries, POST for
// mutations; result is unmarshalled into out.
func (s *Streams) call(path string, payload any, mutation bool, out any) error {
	u := s.BaseURL + "/api/trpc/" + path
	var (
		req *http.Request
		err error
	)
	if mutation {
		body, _ := json.Marshal(payload)
		req, err = http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	} else {
		inputJSON, _ := json.Marshal(payload)
		q := url.Values{}
		q.Set("input", string(inputJSON))
		req, err = http.NewRequest(http.MethodGet, u+"?"+q.Encode(), nil)
	}
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+s.APIKey)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tRPC %s %d: %s", path, resp.StatusCode, truncate(string(body), 200))
	}
	var env struct {
		Result *struct {
			Data json.RawMessage `json:"data"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("tRPC %s decode: %w", path, err)
	}
	if env.Error != nil {
		return errors.New(env.Error.Message)
	}
	if env.Result == nil {
		return fmt.Errorf("tRPC %s: empty result", path)
	}
	return json.Unmarshal(env.Result.Data, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ── live inputs ─────────────────────────────────────────────────────────

type LiveInputData struct {
	ID              string `json:"id"`
	ExternalInputID string `json:"external_input_id"`
	Name            string `json:"name"`
	Status          string `json:"status"`            // idle | live | errored
	Ingest          string `json:"ingest"`            // fmp4 | whip | rtmp | srt
	CreatedAt       string `json:"createdAt"`
}

// SignedPlaybackURL is what mintPlaybackToken returns. The URL is ready to
// hand to BroadcastViewer.Open.
type SignedPlaybackURL struct {
	URL       string // moq://relay/playback/<external_input_id>?tok=<jwt>
	Kid       string
	Alg       string
	ExpiresAt int64 // unix seconds
}

// LiveInputWithSecret is returned by Create + RotateStreamKey. The cleartext
// stream key is only available on issuance — the BFF stores only a hash.
type LiveInputWithSecret struct {
	Input     *LiveInput
	StreamKey string
}

type LiveInputs struct{ s *Streams }

func (li *LiveInputs) Create(name, ingest string) (*LiveInputWithSecret, error) {
	if li.s.OrgID == "" {
		return nil, errors.New("streams.liveInputs.Create: OrgID required on Streams")
	}
	if ingest == "" {
		ingest = "fmp4"
	}
	var row struct {
		LiveInputData
		StreamKeyCleartext string `json:"stream_key_cleartext"`
	}
	if err := li.s.call("streams.liveInputs.create",
		map[string]any{"orgId": li.s.OrgID, "name": name, "ingest": ingest},
		true, &row); err != nil {
		return nil, err
	}
	return &LiveInputWithSecret{
		Input:     &LiveInput{s: li.s, LiveInputData: row.LiveInputData},
		StreamKey: row.StreamKeyCleartext,
	}, nil
}

func (li *LiveInputs) Get(id string) (*LiveInput, error) {
	if li.s.OrgID == "" {
		return nil, errors.New("streams.liveInputs.Get: OrgID required on Streams")
	}
	var row LiveInputData
	if err := li.s.call("streams.liveInputs.get",
		map[string]any{"orgId": li.s.OrgID, "id": id},
		false, &row); err != nil {
		return nil, err
	}
	return &LiveInput{s: li.s, LiveInputData: row}, nil
}

// LiveInput is a snapshot of a stream_live_input row + bound methods.
type LiveInput struct {
	LiveInputData
	s *Streams
}

// SignedPlaybackURL mints a short-lived playback URL for this input. The
// relay verifies the JWT inside the SUBSCRIBE handler.
func (l *LiveInput) SignedPlaybackURL(ttlSeconds int) (*SignedPlaybackURL, error) {
	if l.s.OrgID == "" {
		return nil, errors.New("LiveInput.SignedPlaybackURL: OrgID required on Streams")
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 3600
	}
	var resp struct {
		Token     string `json:"token"`
		Kid       string `json:"kid"`
		Alg       string `json:"alg"`
		ExpiresAt int64  `json:"expires_at"`
		Input     string `json:"input"`
	}
	if err := l.s.call("streams.liveInputs.mintPlaybackToken",
		map[string]any{"orgId": l.s.OrgID, "id": l.ID, "ttlSeconds": ttlSeconds},
		true, &resp); err != nil {
		return nil, err
	}
	return &SignedPlaybackURL{
		URL:       fmt.Sprintf("moq://relay.clutchcall.dev/playback/%s?tok=%s", resp.Input, resp.Token),
		Kid:       resp.Kid,
		Alg:       resp.Alg,
		ExpiresAt: resp.ExpiresAt,
	}, nil
}

func (l *LiveInput) RotateStreamKey() (*LiveInputWithSecret, error) {
	if l.s.OrgID == "" {
		return nil, errors.New("LiveInput.RotateStreamKey: OrgID required on Streams")
	}
	var row struct {
		LiveInputData
		StreamKeyCleartext string `json:"stream_key_cleartext"`
	}
	if err := l.s.call("streams.liveInputs.rotateStreamKey",
		map[string]any{"orgId": l.s.OrgID, "id": l.ID},
		true, &row); err != nil {
		return nil, err
	}
	return &LiveInputWithSecret{
		Input:     &LiveInput{s: l.s, LiveInputData: row.LiveInputData},
		StreamKey: row.StreamKeyCleartext,
	}, nil
}

// ── signing keys ────────────────────────────────────────────────────────

type SigningKeyData struct {
	ID           string `json:"id"`
	Alg          string `json:"alg"` // Ed25519 | RS256
	Use          string `json:"use"` // playback
	PublicKeyPem string `json:"publicKeyPem"`
	Status       string `json:"status"`
	CreatedAt    string `json:"createdAt"`
}

type SigningKeys struct{ s *Streams }

func (sk *SigningKeys) Create(alg, use string) (*SigningKeyData, error) {
	if sk.s.OrgID == "" {
		return nil, errors.New("streams.signingKeys.Create: OrgID required on Streams")
	}
	if alg == "" {
		alg = "Ed25519"
	}
	if use == "" {
		use = "playback"
	}
	var row SigningKeyData
	if err := sk.s.call("streams.signingKeys.create",
		map[string]any{"orgId": sk.s.OrgID, "alg": alg, "use": use},
		true, &row); err != nil {
		return nil, err
	}
	return &row, nil
}

// ── broadcast viewer ────────────────────────────────────────────────────

// CloseReason captures why a viewer ended.
type CloseReason string

const (
	CloseComplete     CloseReason = "complete"
	CloseAuthFailed   CloseReason = "auth_failed"
	CloseNetwork      CloseReason = "network"
	CloseClosedByCaller CloseReason = "closed_by_caller"
)

// BroadcastChunk is what every chunk callback receives. The first chunk
// (isInit == true) is the CMAF initialization segment.
type BroadcastChunk struct {
	Data        []byte
	TimestampUs uint64
	Priority    uint8
	IsInit      bool
}

// BroadcastViewer wraps a MoqtClient subscription to a playback URL. The
// SDK takes care of parsing the URL, picking the right namespace, and
// translating relay close codes into CloseReason.
type BroadcastViewer struct {
	client *clutchcall.MoqtClient
	sub    *clutchcall.FrameSubscription
}

// Open a signed playback URL and forward chunks to onChunk in arrival
// order. onClose fires exactly once when the viewer ends.
func Open(
	moqURL string,
	onChunk func(isInit bool, c BroadcastChunk),
	onClose func(reason CloseReason, detail string),
) (*BroadcastViewer, error) {
	if onChunk == nil {
		return nil, errors.New("BroadcastViewer.Open: onChunk required")
	}
	wtURL, token, namespace, err := parsePlaybackURL(moqURL)
	if err != nil {
		return nil, err
	}
	var sawInit bool
	closed := false
	fire := func(r CloseReason, detail string) {
		if closed || onClose == nil {
			return
		}
		closed = true
		onClose(r, detail)
	}
	client, err := clutchcall.ConnectMoqt(wtURL, token, func(state int) {
		if state == 3 { // Closed
			fire(CloseNetwork, "")
		} else if state == 4 { // Failed
			fire(CloseAuthFailed, "")
		}
	})
	if err != nil {
		return nil, err
	}
	sub, err := client.SubscribeFrame(namespace, "broadcast", func(ts uint64, prio uint8, data []byte) {
		isInit := !sawInit
		sawInit = true
		onChunk(isInit, BroadcastChunk{
			Data:        data,
			TimestampUs: ts,
			Priority:    prio,
			IsInit:      isInit,
		})
	})
	if err != nil {
		client.Close()
		return nil, err
	}
	return &BroadcastViewer{client: client, sub: sub}, nil
}

func (v *BroadcastViewer) Close() {
	if v.sub != nil {
		v.sub.Close()
		v.sub = nil
	}
	if v.client != nil {
		v.client.Close()
		v.client = nil
	}
}

// ── broadcast publisher ─────────────────────────────────────────────────

// PublisherCodecs carries RFC 6381 codec hints; optional but recommended.
type PublisherCodecs struct {
	Video string // e.g. "avc1.42E01F"
	Audio string // e.g. "opus" or "mp4a.40.2"
}

type PublisherCloseReason string

const (
	PubClosedByCaller PublisherCloseReason = "closed_by_caller"
	PubAuthFailed     PublisherCloseReason = "auth_failed"
	PubNetwork        PublisherCloseReason = "network"
	PubFinished       PublisherCloseReason = "finished"
)

// BroadcastPublisher pushes a broadcast into the relay using the per-input
// stream key. Auth is by the stream key — there is no playback JWT on the
// publisher side; ownership is proven by the key the relay resolves at
// CONNECT time.
type BroadcastPublisher struct {
	client     *clutchcall.MoqtClient
	track      *clutchcall.FramePublication
	wroteInit  bool
	onClose    func(PublisherCloseReason, string)
	closed     bool
}

// PublishOpen connects a publisher for the given (inputID, streamKey) pair.
func PublishOpen(
	inputID, streamKey string,
	codecs PublisherCodecs,
	relayHost string,
	onClose func(reason PublisherCloseReason, detail string),
) (*BroadcastPublisher, error) {
	if inputID == "" {
		return nil, errors.New("BroadcastPublisher: inputID required")
	}
	if streamKey == "" {
		return nil, errors.New("BroadcastPublisher: streamKey required")
	}
	if relayHost == "" {
		relayHost = "relay.clutchcall.dev"
	}
	moqURL := fmt.Sprintf("moq://%s/publish/%s?sk=%s",
		relayHost, inputID, url.QueryEscape(streamKey))
	closed := false
	fire := func(r PublisherCloseReason, detail string) {
		if closed || onClose == nil {
			return
		}
		closed = true
		onClose(r, detail)
	}
	client, err := clutchcall.ConnectMoqt(moqURL, "", func(state int) {
		if state == 3 {
			fire(PubNetwork, "")
		} else if state == 4 {
			fire(PubAuthFailed, "")
		}
	})
	if err != nil {
		return nil, err
	}
	schemaTag := strings.TrimRight(codecs.Video+","+codecs.Audio, ",")
	track, err := client.PublishFrame(
		"publish/"+inputID, "broadcast",
		"media.broadcast", schemaTag, 0,
	)
	if err != nil {
		client.Close()
		return nil, err
	}
	return &BroadcastPublisher{
		client:  client,
		track:   track,
		onClose: onClose,
	}, nil
}

// Write pushes one chunk. The first call is treated as the CMAF init
// segment (priority 0, fresh group); subsequent calls are media segments
// (priority 1). Pass timestampUs > 0 to override the monotonic default.
func (p *BroadcastPublisher) Write(chunk []byte, timestampUs uint64) {
	if timestampUs == 0 {
		timestampUs = monotonicUs()
	}
	priority := uint8(1)
	if !p.wroteInit {
		p.wroteInit = true
		priority = 0
	}
	p.track.Write(timestampUs, chunk, priority)
}

func (p *BroadcastPublisher) Close(reason PublisherCloseReason) {
	if p.closed {
		return
	}
	p.closed = true
	if p.track != nil {
		p.track.Close()
	}
	if p.client != nil {
		p.client.Close()
	}
	if p.onClose != nil {
		p.onClose(reason, "")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

func parsePlaybackURL(moqURL string) (wtURL, token, namespace string, err error) {
	if !strings.HasPrefix(moqURL, "moq://") {
		return "", "", "", fmt.Errorf("BroadcastViewer: expected moq:// URL, got %q", truncate(moqURL, 32))
	}
	asHTTPS, err := url.Parse(strings.Replace(moqURL, "moq://", "https://", 1))
	if err != nil {
		return "", "", "", err
	}
	parts := strings.Split(strings.Trim(asHTTPS.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "playback" {
		return "", "", "", errors.New("BroadcastViewer: playback URL must be moq://<host>/playback/<input_id>?tok=…")
	}
	token = asHTTPS.Query().Get("tok")
	if token == "" {
		return "", "", "", errors.New("BroadcastViewer: playback URL is missing ?tok=<jwt>")
	}
	return moqURL, token, "playback/" + parts[1], nil
}

// monotonicUs returns a monotonic timestamp in microseconds for the writer.
func monotonicUs() uint64 {
	// stdlib time.Now() is fine for this — we just need a monotonic source
	// of microseconds for MoQT object ordering.
	return uint64(timeNowUnixNano() / 1000)
}
