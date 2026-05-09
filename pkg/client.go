package clutchcall

/*
#cgo LDFLAGS: -lclutchcall_core_ffi
#include <stdlib.h>
#include <stdbool.h>
#include <stdint.h>

typedef struct { void* data; size_t length; } ClutchCallBuffer;

typedef struct {
    char* call_sid; char* payload; char* codec; uint64_t sequence_number; bool end_of_stream;
} C_AudioFrame;

typedef struct {
    char* call_sid; int32_t event_type; char* status; int64_t start_timestamp_ms; int32_t q850_cause; char* recording_url; int32_t duration_seconds;
} C_CallEvent;

// Full Core Mappings
extern ClutchCallBuffer clutchcall_rpc_originate_request(const char*, const char*, const char*, const char*, const char*, const char*, int, const char*, int, const char*, bool, int, const char*);
extern ClutchCallBuffer clutchcall_rpc_bulk_request(const char*, const char*, const char*, const char*, const char*, const char*, const char*, int, int, const char*, int, int, const char*, bool, int, const char*);
extern ClutchCallBuffer clutchcall_rpc_terminate_request(const char*);
extern ClutchCallBuffer clutchcall_rpc_abort_bulk_request(const char*);
extern ClutchCallBuffer clutchcall_rpc_event_stream_request(const char*);
extern ClutchCallBuffer clutchcall_rpc_barge_request(const char*);
extern ClutchCallBuffer clutchcall_rpc_set_inbound_routing_request(const char*, int, const char*, const char*, const char*, const char*);
extern ClutchCallBuffer clutchcall_rpc_get_incoming_calls_request(const char*);
extern ClutchCallBuffer clutchcall_rpc_answer_incoming_call_request(const char*, const char*, const char*, const char*);
extern ClutchCallBuffer clutchcall_rpc_empty();
extern ClutchCallBuffer clutchcall_rpc_bucket_request(const char*);
extern ClutchCallBuffer clutchcall_rpc_bucket_action_request(const char*, int);

extern void clutchcall_free_buffer(ClutchCallBuffer buf);
extern C_AudioFrame clutchcall_deserialize_audio_frame(const void*, size_t);
extern C_CallEvent clutchcall_deserialize_call_event(const void*, size_t);
extern ClutchCallBuffer clutchcall_serialize_audio_frame(const char*, const char*, const char*, uint64_t, bool);
*/
import "C"

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unsafe"
    "github.com/google/uuid"
	"github.com/quic-go/quic-go"
)



type Credentials struct {
	TenantID     string `json:"tenant_id"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
}

type Client struct {
	endpoint string
	creds    Credentials
	conn     quic.Connection
    clientId string

	OnAudioFrame func(payload []byte)
	OnCallEvent  func(payload []byte)

	audioOutStream quic.SendStream
}

func NewClient(endpoint string, credPath string) (*Client, error) {
	if credPath == "" {
		credPath = os.Getenv("CLUTCHCALL_CREDENTIALS")
	}

	data, err := os.ReadFile(credPath)
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("json config: %w", err)
	}

	return &Client{
		endpoint: endpoint,
		creds:    creds,
        clientId: uuid.New().String(),
	}, nil
}

func (c *Client) connect() error {
	hostPort := strings.TrimPrefix(c.endpoint, "quic://")
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h3"},
	}
	conn, err := quic.DialAddr(context.Background(), hostPort, tlsConf, nil)
	if err != nil {
		return err
	}
	c.conn = conn

	go c.receiveLoop()

    cIdStr := C.CString(c.clientId)
    defer C.free(unsafe.Pointer(cIdStr))
    c.sendRPC(C.clutchcall_rpc_event_stream_request(cIdStr))

	return nil
}

func (c *Client) receiveLoop() {
	for {
		stream, err := c.conn.AcceptUniStream(context.Background())
		if err != nil {
			return 
		}
		go func(s quic.ReceiveStream) {
			for {
				lengthBuf := make([]byte, 4)
				if _, err := io.ReadFull(s, lengthBuf); err != nil {
					return
				}
				totalLen := binary.LittleEndian.Uint32(lengthBuf)
				if totalLen == 0 || totalLen > 1024*1024 {
					continue 
				}

				payloadBuf := make([]byte, totalLen)
				if _, err := io.ReadFull(s, payloadBuf); err != nil {
					return
				}

				if totalLen >= 4 {
					dgID := binary.LittleEndian.Uint32(payloadBuf[0:4])
					payloadData := payloadBuf[4:]

					if dgID == uint32(MethodID_AUDIO_FRAME) {
						if c.OnAudioFrame != nil {
							c.OnAudioFrame(payloadData)
						}
					} else if dgID == MethodID_STREAM_EVENTS {
						if c.OnCallEvent != nil {
							c.OnCallEvent(payloadData)
						}
					}
				}
			}
		}(stream)
	}
}

func (c *Client) sendRPC(buf C.ClutchCallBuffer) error {
	defer C.clutchcall_free_buffer(buf)

	if c.conn == nil {
		if err := c.connect(); err != nil {
			return err
		}
	}

	length := uint32(buf.length)
	payloadBytes := C.GoBytes(buf.data, C.int(length))

	stream, err := c.conn.OpenStreamSync(context.Background())
	if err != nil {
		return err
	}
	defer stream.Close()

	stream.Write(payloadBytes)
	return nil
}

func (c *Client) PushAudio(callSid string, payload []byte, codec string, sequenceNumber uint64, endOfStream bool) error {
	if c.conn == nil {
		if err := c.connect(); err != nil {
			return err
		}
	}
	
	packet := c.SerializeAudioFrame(callSid, payload, codec, sequenceNumber, endOfStream)
	
	if c.audioOutStream == nil {
		stream, err := c.conn.OpenUniStreamSync(context.Background())
		if err != nil {
			return err
		}
		c.audioOutStream = stream
	}
	
	_, err := c.audioOutStream.Write(packet)
	return err
}

// Full Method Mappings Implementation

func (c *Client) Dial(to, trunkId, callFrom string, maxDurationMs int, defaultApp int, defaultAppArgs string, aiWs string, aiQuic string, autoBargeIn bool, bargeInPatienceMs int, clientIdOverride string) error {
	cTrunk, cTo, cFrom := C.CString(trunkId), C.CString(to), C.CString(callFrom)
	cEmpty, cToken := C.CString(""), C.CString(c.creds.TenantID)
	cArgs := C.CString(defaultAppArgs)
	cWs, cQuic := C.CString(aiWs), C.CString(aiQuic)
	cBargeIn := C.bool(autoBargeIn)
	
	targetClientId := c.clientId
	if clientIdOverride != "" {
		targetClientId = clientIdOverride
	}
    cClient := C.CString(targetClientId)

	defer C.free(unsafe.Pointer(cTrunk))
	defer C.free(unsafe.Pointer(cTo))
	defer C.free(unsafe.Pointer(cFrom))
	defer C.free(unsafe.Pointer(cEmpty))
	defer C.free(unsafe.Pointer(cToken))
	defer C.free(unsafe.Pointer(cArgs))
	defer C.free(unsafe.Pointer(cWs))
	defer C.free(unsafe.Pointer(cQuic))
    defer C.free(unsafe.Pointer(cClient))

	buf := C.clutchcall_rpc_originate_request(cTrunk, cTo, cFrom, cWs, cQuic, cToken, C.int(maxDurationMs), cEmpty, C.int(defaultApp), cArgs, cBargeIn, C.int(bargeInPatienceMs), cClient)
	return c.sendRPC(buf)
}

func (c *Client) OriginateBulk(csvUrl, trunkId, campaignId string, cps int, defaultApp int, defaultAppArgs string, aiWs string, aiQuic string, autoBargeIn bool, bargeInPatienceMs int) error {
	cCsv, cTrunk, cCmp := C.CString(csvUrl), C.CString(trunkId), C.CString(campaignId)
	cEmpty, cToken := C.CString(""), C.CString(c.creds.TenantID)
	cArgs := C.CString(defaultAppArgs)
	cWs, cQuic := C.CString(aiWs), C.CString(aiQuic)
	cBargeIn := C.bool(autoBargeIn)

	defer C.free(unsafe.Pointer(cCsv))
	defer C.free(unsafe.Pointer(cTrunk))
	defer C.free(unsafe.Pointer(cCmp))
	defer C.free(unsafe.Pointer(cEmpty))
	defer C.free(unsafe.Pointer(cToken))
	defer C.free(unsafe.Pointer(cArgs))
	defer C.free(unsafe.Pointer(cWs))
	defer C.free(unsafe.Pointer(cQuic))
    cClient := C.CString(c.clientId)
    defer C.free(unsafe.Pointer(cClient))

	buf := C.clutchcall_rpc_bulk_request(cCsv, cTrunk, cEmpty, cEmpty, cWs, cQuic, cToken, C.int(0), C.int(defaultApp), cArgs, C.int(cps), C.int(cps), cCmp, cBargeIn, C.int(bargeInPatienceMs), cClient)
	return c.sendRPC(buf)
}

func (c *Client) Terminate(callSid string) error {
	cSid := C.CString(callSid)
	defer C.free(unsafe.Pointer(cSid))
	buf := C.clutchcall_rpc_terminate_request(cSid)
	return c.sendRPC(buf)
}

func (c *Client) Barge(callSid string) error {
	cSid := C.CString(callSid)
	defer C.free(unsafe.Pointer(cSid))
	buf := C.clutchcall_rpc_barge_request(cSid)
	return c.sendRPC(buf)
}

func (c *Client) AbortBulk(campaignId string) error {
	cCmp := C.CString(campaignId)
	defer C.free(unsafe.Pointer(cCmp))
	buf := C.clutchcall_rpc_abort_bulk_request(cCmp)
	return c.sendRPC(buf)
}

func (c *Client) StreamEvents(clientId string) error {
	cId := C.CString(clientId)
	defer C.free(unsafe.Pointer(cId))
	buf := C.clutchcall_rpc_event_stream_request(cId)
	return c.sendRPC(buf)
}

func (c *Client) SetInboundRouting(trunkId string, rule int, audioUrl, webhookUrl, aiWs, aiQuic string) error {
	cTrunk, cAudio, cWeb, cAiWs, cAiQuic := C.CString(trunkId), C.CString(audioUrl), C.CString(webhookUrl), C.CString(aiWs), C.CString(aiQuic)
	defer C.free(unsafe.Pointer(cTrunk))
	defer C.free(unsafe.Pointer(cAudio))
	defer C.free(unsafe.Pointer(cWeb))
	defer C.free(unsafe.Pointer(cAiWs))
	defer C.free(unsafe.Pointer(cAiQuic))

	buf := C.clutchcall_rpc_set_inbound_routing_request(cTrunk, C.int(rule), cAudio, cWeb, cAiWs, cAiQuic)
	return c.sendRPC(buf)
}

func (c *Client) GetIncomingCalls(trunkId string) error {
	cTrunk := C.CString(trunkId)
	defer C.free(unsafe.Pointer(cTrunk))
	buf := C.clutchcall_rpc_get_incoming_calls_request(cTrunk)
	return c.sendRPC(buf)
}

func (c *Client) AnswerIncomingCall(callSid, aiWs, aiQuic string) error {
	cSid, cAiWs, cAiQuic := C.CString(callSid), C.CString(aiWs), C.CString(aiQuic)
	defer C.free(unsafe.Pointer(cSid))
	defer C.free(unsafe.Pointer(cAiWs))
	defer C.free(unsafe.Pointer(cAiQuic))
	cClient := C.CString(c.clientId)
	defer C.free(unsafe.Pointer(cClient))
	buf := C.clutchcall_rpc_answer_incoming_call_request(cSid, cAiWs, cAiQuic, cClient)
	return c.sendRPC(buf)
}

func (c *Client) GetActiveBuckets() error {
	buf := C.clutchcall_rpc_empty()
	return c.sendRPC(buf)
}

func (c *Client) GetBucketCalls(bucketId string) error {
	cId := C.CString(bucketId)
	defer C.free(unsafe.Pointer(cId))
	buf := C.clutchcall_rpc_bucket_request(cId)
	return c.sendRPC(buf)
}

func (c *Client) ExecuteBucketAction(bucketId string, action int) error {
	cId := C.CString(bucketId)
	defer C.free(unsafe.Pointer(cId))
	buf := C.clutchcall_rpc_bucket_action_request(cId, C.int(action))
	return c.sendRPC(buf)
}

func (c *Client) DeserializeAudioFrame(payload []byte) (string, []byte) {
	cPtr := C.CBytes(payload)
	defer C.free(cPtr)

	frame := C.clutchcall_deserialize_audio_frame(cPtr, C.size_t(len(payload)))
	return C.GoString(frame.call_sid), C.GoBytes(unsafe.Pointer(frame.payload), 160)
}

func (c *Client) DeserializeCallEvent(payload []byte) (string, string) {
	cPtr := C.CBytes(payload)
	defer C.free(cPtr)

	evt := C.clutchcall_deserialize_call_event(cPtr, C.size_t(len(payload)))
	return C.GoString(evt.call_sid), C.GoString(evt.status)
}

func (c *Client) SerializeAudioFrame(callSid string, payload []byte, codec string, sequenceNumber uint64, endOfStream bool) []byte {
	cSid := C.CString(callSid)
	cCodec := C.CString(codec)
	cPayload := C.CString(string(payload))
	defer C.free(unsafe.Pointer(cSid))
	defer C.free(unsafe.Pointer(cCodec))
	defer C.free(unsafe.Pointer(cPayload))

	buf := C.clutchcall_serialize_audio_frame(cSid, cPayload, cCodec, C.uint64_t(sequenceNumber), C.bool(endOfStream))
	bytes := C.GoBytes(buf.data, C.int(buf.length))
	C.clutchcall_free_buffer(buf)

	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(bytes)) + 4)
	binary.LittleEndian.PutUint32(header[4:8], uint32(MethodID_AUDIO_FRAME))
	return append(header, bytes...)
}
