package clutchcall

// Capability-aware MoQT track pub/sub for Go, over the same C++ engine
// (core/moqt_client.cc) the C++/Python SDKs use, via core/moqt_ffi.cc. A
// published track carries a `capability` (intent/routing key); the relay/gateway
// routes it to the module that registered that capability.
//
//	c, _ := ConnectMoqt("quic://relay.acme.dev:4443", token, func(s int) {})
//	pub, _ := c.PublishAudio("voice/acme/call-1", "mic", "asr", 48000, 1, 20)
//	pub.Write(tsUs, pcm)
//	sub, _ := c.SubscribeAudio("voice/acme/call-1", "agent", func(ts uint64, b []byte) {})
//
// Callbacks fire from the engine's io_thread; we route them back to Go via
// runtime/cgo.Handle (the C `user` pointer carries the handle).

/*
#cgo LDFLAGS: -lclutchcall_moqt_ffi
#include <stdint.h>
#include <stdlib.h>

typedef struct clutch_moqt_client_t clutch_moqt_client_t;
typedef struct clutch_moqt_pub_t    clutch_moqt_pub_t;
typedef struct clutch_moqt_sub_t    clutch_moqt_sub_t;
typedef void (*clutch_moqt_state_cb)(void*, int32_t);
typedef void (*clutch_moqt_frame_cb)(void*, uint64_t, const uint8_t*, size_t);
typedef struct clutch_moqt_frame_pub_t clutch_moqt_frame_pub_t;
typedef struct clutch_moqt_frame_sub_t clutch_moqt_frame_sub_t;
typedef void (*clutch_moqt_frame_obj_cb)(void*, uint64_t, uint8_t, const uint8_t*, size_t);

extern clutch_moqt_client_t* clutch_moqt_connect(const char*, const char*, clutch_moqt_state_cb, void*);
extern void clutch_moqt_client_close(clutch_moqt_client_t*);
extern clutch_moqt_pub_t* clutch_moqt_publish_audio(clutch_moqt_client_t*, const char*, const char*, const char*, uint32_t, uint8_t, uint16_t);
extern void clutch_moqt_pub_write(clutch_moqt_pub_t*, uint64_t, const uint8_t*, size_t);
extern size_t clutch_moqt_pub_subscriber_count(clutch_moqt_pub_t*);
extern void clutch_moqt_pub_close(clutch_moqt_pub_t*);
extern clutch_moqt_sub_t* clutch_moqt_subscribe_audio(clutch_moqt_client_t*, const char*, const char*, clutch_moqt_frame_cb, void*);
extern void clutch_moqt_sub_close(clutch_moqt_sub_t*);
extern clutch_moqt_frame_pub_t* clutch_moqt_publish_frame(clutch_moqt_client_t*, const char*, const char*, const char*, const char*, uint8_t);
extern void clutch_moqt_frame_write(clutch_moqt_frame_pub_t*, uint64_t, const uint8_t*, size_t, uint8_t);
extern size_t clutch_moqt_frame_pub_subscriber_count(clutch_moqt_frame_pub_t*);
extern void clutch_moqt_frame_pub_close(clutch_moqt_frame_pub_t*);
extern clutch_moqt_frame_sub_t* clutch_moqt_subscribe_frame(clutch_moqt_client_t*, const char*, const char*, clutch_moqt_frame_obj_cb, void*);
extern void clutch_moqt_frame_sub_close(clutch_moqt_frame_sub_t*);

// cgo can't pass a Go func as a C function pointer; route through static C
// trampolines that call the //export'd Go functions.
extern void goMoqtFrame(void*, uint64_t, uint8_t*, size_t);
extern void goMoqtState(void*, int32_t);
static void cc_moqt_frame_tramp(void* u, uint64_t ts, const uint8_t* d, size_t n) { goMoqtFrame(u, ts, (uint8_t*)d, n); }
static void cc_moqt_state_tramp(void* u, int32_t s) { goMoqtState(u, s); }
static clutch_moqt_frame_cb cc_moqt_frame_cb(void) { return cc_moqt_frame_tramp; }
static clutch_moqt_state_cb cc_moqt_state_cb(void) { return cc_moqt_state_tramp; }
extern void goMoqtFrameObj(void*, uint64_t, uint8_t, uint8_t*, size_t);
static void cc_moqt_frame_obj_tramp(void* u, uint64_t ts, uint8_t prio, const uint8_t* d, size_t n) { goMoqtFrameObj(u, ts, prio, (uint8_t*)d, n); }
static clutch_moqt_frame_obj_cb cc_moqt_frame_obj_cb(void) { return cc_moqt_frame_obj_tramp; }
*/
import "C"

import (
	"errors"
	"runtime/cgo"
	"unsafe"
)

//export goMoqtFrame
func goMoqtFrame(user unsafe.Pointer, ts C.uint64_t, data *C.uint8_t, n C.size_t) {
	if user == nil {
		return
	}
	h := cgo.Handle(uintptr(user))
	if cb, ok := h.Value().(func(uint64, []byte)); ok {
		buf := C.GoBytes(unsafe.Pointer(data), C.int(n))
		cb(uint64(ts), buf)
	}
}

//export goMoqtState
func goMoqtState(user unsafe.Pointer, st C.int32_t) {
	if user == nil {
		return
	}
	h := cgo.Handle(uintptr(user))
	if cb, ok := h.Value().(func(int)); ok {
		cb(int(st))
	}
}

//export goMoqtFrameObj
func goMoqtFrameObj(user unsafe.Pointer, ts C.uint64_t, prio C.uint8_t, data *C.uint8_t, n C.size_t) {
	if user == nil {
		return
	}
	h := cgo.Handle(uintptr(user))
	if cb, ok := h.Value().(func(uint64, uint8, []byte)); ok {
		buf := C.GoBytes(unsafe.Pointer(data), C.int(n))
		cb(uint64(ts), uint8(prio), buf)
	}
}

// MoqtClient is a MoQT session against the relay.
type MoqtClient struct {
	h      *C.clutch_moqt_client_t
	stateH cgo.Handle
}

// AudioPublication is a live published audio track.
type AudioPublication struct{ h *C.clutch_moqt_pub_t }

// AudioSubscription is a live subscription; frames arrive on the on_frame cb.
type AudioSubscription struct {
	h      *C.clutch_moqt_sub_t
	frameH cgo.Handle
}

// ConnectMoqt opens a MoQT session. onState (optional) reports
// ConnectionStateKind transitions (0=Connecting,1=Connected,...,4=Failed).
func ConnectMoqt(url, token string, onState func(int)) (*MoqtClient, error) {
	cu, ct := C.CString(url), C.CString(token)
	defer C.free(unsafe.Pointer(cu))
	defer C.free(unsafe.Pointer(ct))
	var sh cgo.Handle
	var user unsafe.Pointer
	if onState != nil {
		sh = cgo.NewHandle(onState)
		user = unsafe.Pointer(uintptr(sh)) //nolint:govet // cgo.Handle as opaque token
	}
	h := C.clutch_moqt_connect(cu, ct, C.cc_moqt_state_cb(), user)
	if h == nil {
		if onState != nil {
			sh.Delete()
		}
		return nil, errors.New("clutch_moqt_connect failed")
	}
	return &MoqtClient{h: h, stateH: sh}, nil
}

// PublishAudio starts a capability-tagged audio track.
func (c *MoqtClient) PublishAudio(namespace, name, capability string,
	sampleRate uint32, channels uint8, frameMs uint16) (*AudioPublication, error) {
	cn, cm, cc := C.CString(namespace), C.CString(name), C.CString(capability)
	defer C.free(unsafe.Pointer(cn))
	defer C.free(unsafe.Pointer(cm))
	defer C.free(unsafe.Pointer(cc))
	h := C.clutch_moqt_publish_audio(c.h, cn, cm, cc,
		C.uint32_t(sampleRate), C.uint8_t(channels), C.uint16_t(frameMs))
	if h == nil {
		return nil, errors.New("publish_audio failed")
	}
	return &AudioPublication{h: h}, nil
}

// Write enqueues one frame's PCM bytes.
func (p *AudioPublication) Write(timestampUs uint64, data []byte) {
	if p.h == nil {
		return
	}
	var ptr *C.uint8_t
	if len(data) > 0 {
		ptr = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}
	C.clutch_moqt_pub_write(p.h, C.uint64_t(timestampUs), ptr, C.size_t(len(data)))
}

func (p *AudioPublication) SubscriberCount() int {
	if p.h == nil {
		return 0
	}
	return int(C.clutch_moqt_pub_subscriber_count(p.h))
}

func (p *AudioPublication) Close() {
	if p.h != nil {
		C.clutch_moqt_pub_close(p.h)
		p.h = nil
	}
}

// SubscribeAudio subscribes to a track; onFrame(ts_us, pcm) fires per object.
func (c *MoqtClient) SubscribeAudio(namespace, name string,
	onFrame func(uint64, []byte)) (*AudioSubscription, error) {
	cn, cm := C.CString(namespace), C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	defer C.free(unsafe.Pointer(cm))
	fh := cgo.NewHandle(onFrame)
	h := C.clutch_moqt_subscribe_audio(c.h, cn, cm, C.cc_moqt_frame_cb(),
		unsafe.Pointer(uintptr(fh))) //nolint:govet
	if h == nil {
		fh.Delete()
		return nil, errors.New("subscribe_audio failed")
	}
	return &AudioSubscription{h: h, frameH: fh}, nil
}

func (s *AudioSubscription) Close() {
	if s.h != nil {
		C.clutch_moqt_sub_close(s.h)
		s.h = nil
		s.frameH.Delete()
	}
}

// FramePublication is a live published frame track (opaque binary, per-frame
// priority) — robot telemetry / game state.
type FramePublication struct{ h *C.clutch_moqt_frame_pub_t }

// FrameSubscription is a live frame subscription.
type FrameSubscription struct {
	h      *C.clutch_moqt_frame_sub_t
	frameH cgo.Handle
}

// PublishFrame starts a capability-tagged frame track.
func (c *MoqtClient) PublishFrame(namespace, name, capability, schemaTag string,
	defaultPriority uint8) (*FramePublication, error) {
	cn, cm := C.CString(namespace), C.CString(name)
	cc, cs := C.CString(capability), C.CString(schemaTag)
	defer C.free(unsafe.Pointer(cn))
	defer C.free(unsafe.Pointer(cm))
	defer C.free(unsafe.Pointer(cc))
	defer C.free(unsafe.Pointer(cs))
	h := C.clutch_moqt_publish_frame(c.h, cn, cm, cc, cs, C.uint8_t(defaultPriority))
	if h == nil {
		return nil, errors.New("publish_frame failed")
	}
	return &FramePublication{h: h}, nil
}

// Write enqueues one frame with a per-frame priority.
func (p *FramePublication) Write(timestampUs uint64, data []byte, priority uint8) {
	if p.h == nil {
		return
	}
	var ptr *C.uint8_t
	if len(data) > 0 {
		ptr = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}
	C.clutch_moqt_frame_write(p.h, C.uint64_t(timestampUs), ptr, C.size_t(len(data)), C.uint8_t(priority))
}

func (p *FramePublication) SubscriberCount() int {
	if p.h == nil {
		return 0
	}
	return int(C.clutch_moqt_frame_pub_subscriber_count(p.h))
}

func (p *FramePublication) Close() {
	if p.h != nil {
		C.clutch_moqt_frame_pub_close(p.h)
		p.h = nil
	}
}

// SubscribeFrame subscribes to a frame track; onFrame(ts_us, priority, data)
// fires per object.
func (c *MoqtClient) SubscribeFrame(namespace, name string,
	onFrame func(uint64, uint8, []byte)) (*FrameSubscription, error) {
	cn, cm := C.CString(namespace), C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	defer C.free(unsafe.Pointer(cm))
	fh := cgo.NewHandle(onFrame)
	h := C.clutch_moqt_subscribe_frame(c.h, cn, cm, C.cc_moqt_frame_obj_cb(),
		unsafe.Pointer(uintptr(fh))) //nolint:govet
	if h == nil {
		fh.Delete()
		return nil, errors.New("subscribe_frame failed")
	}
	return &FrameSubscription{h: h, frameH: fh}, nil
}

func (s *FrameSubscription) Close() {
	if s.h != nil {
		C.clutch_moqt_frame_sub_close(s.h)
		s.h = nil
		s.frameH.Delete()
	}
}

func (c *MoqtClient) Close() {
	if c.h != nil {
		C.clutch_moqt_client_close(c.h)
		c.h = nil
		if c.stateH != 0 {
			c.stateH.Delete()
		}
	}
}
