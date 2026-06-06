# ClutchCall Go SDK

The official Go wrapper for ClutchCall. **Modality-oriented**: each modality
is its own subpackage, all riding the same MoQT substrate underneath. Pick
the one that matches what you're building; mixing them in one process is fine.

| Subpackage                                  | Modality                                            | Status |
| ------------------------------------------- | --------------------------------------------------- | ------ |
| `github.com/clutchcall/go-sdk/pkg/streams`  | Live broadcasts + signed playback URLs              | **GA** |
| `github.com/clutchcall/go-sdk/pkg/robotics` | Robotics topic pub/sub (ROS 2 CDR)                  | **GA** |
| `github.com/clutchcall/go-sdk/pkg/games`    | Games (rooms, state/input/event channels)           | **GA** |
| `github.com/clutchcall/go-sdk/pkg/data`     | MQTT-style typed pub/sub (`+` / `#` filters)        | **GA** |
| `github.com/clutchcall/go-sdk/pkg/voice`    | Voice (calls + bidirectional audio bridge)          | **GA** |
| `github.com/clutchcall/go-sdk/pkg`          | Legacy voice surface (`ClutchCallClient`) — kept for backwards compat | legacy |

## Installation

```bash
go get github.com/clutchcall/go-sdk
```

## Streams — watch a live broadcast

```go
import "github.com/clutchcall/go-sdk/pkg/streams"

s, _ := streams.New("https://app.clutchcall.dev", os.Getenv("CLUTCHCALL_API_KEY"), "org_abc")
inp, _ := s.LiveInputs().Get("li_xyz")
ticket, _ := inp.SignedPlaybackURL(3600)

v, _ := streams.Open(ticket.URL,
    func(isInit bool, c streams.BroadcastChunk) { /* feed MSE / file writer */ },
    func(reason streams.CloseReason, detail string) { log.Println("closed:", reason) },
)
defer v.Close()
```

## Robotics — telemetry + commands across a fleet

```go
import "github.com/clutchcall/go-sdk/pkg/robotics"

r, _ := robotics.New(token, "turtlebot-7")
odom, _ := r.PublishTelemetry("odom", "nav_msgs/msg/Odometry", robotics.QoSProfile{Reliability: robotics.Reliable})
odom.Write(cdrBytes, 255)  // 255 = use default priority
```

## Games — multiplayer rooms

```go
import "github.com/clutchcall/go-sdk/pkg/games"

// Authoritative server (no playerId)
auth, _ := games.New(token, "duel-42", "")
state, _ := auth.PublishState(30)  // 30 Hz
inputSub, _ := auth.SubscribeInputs(func(pid string, bytes []byte) { /* apply */ })
```

## Data — MQTT-style typed pub/sub

```go
import "github.com/clutchcall/go-sdk/pkg/data"

d, _ := data.New(token, "device-7")
d.Publish("sensors/room1/temperature", []byte("23.5"), false, false)
d.Subscribe("sensors/+/temperature", func(m data.Message) {
    log.Printf("%s ← %s: %s", m.Topic, m.FromClientID, string(m.Payload))
})
```

## Voice — calls + audio bridge

```go
import "github.com/clutchcall/go-sdk/pkg/voice"

v, _    := voice.New("https://app.clutchcall.dev", os.Getenv("CLUTCHCALL_API_KEY"), "org_abc")
call, _ := v.Calls().Originate(voice.OriginateArgs{
    To: "+15551234567", From: "+15558675309",
    TrunkID: "trunk_main", Agent: "healthcare-assistant",
})

bridge, _ := v.AudioBridge().Attach(call.Sid, voice.AudioBridgeOpts{
    Codec: voice.CodecOpus,
    OnUplink: func(frame []byte, tsUs uint64) { asr.Feed(frame) },
})
tts.OnChunk(func(opus []byte) { bridge.PublishDownlink(opus) })

call.Hangup()
```

### Legacy voice surface

The original `pkg.NewClutchCallClient(...)` surface remains available
for backwards compat. Set `CLUTCHCALL_CREDENTIALS` to your
service-account JSON, then:

```go
package main

import (
    "log"

    clutchcall "github.com/clutchcall/go-sdk/pkg"
)

func main() {
    client, err := clutchcall.NewClutchCallClient("pbx.clutchcall.com:443")
    if err != nil {
        log.Fatalf("connect: %v", err)
    }

    // Originate an outbound call.
    if _, err := client.Originate("+1234567890", "wss://my-chatbot.com/media", ""); err != nil {
        log.Fatalf("originate: %v", err)
    }

    // Stream media.
    stream := clutchcall.NewClutchCallAudioStream()
    if err := stream.Connect("wss://pbx.clutchcall.com/media/session_789"); err != nil {
        log.Fatalf("media: %v", err)
    }
    stream.OnAudio(func(pcm []byte) {
        // Forward PCM to your LLM / voice API.
    })
    stream.ReceiveAudioLoop()
}
```

## Native core

The FFI core (`libclutchcall_core_ffi.{so,dylib,dll}`) is loaded at runtime.
Set `CLUTCHCALL_LIB_PATH` if it isn't on the default loader path; see the
[`core-sdk`](https://github.com/clutchcall/core-sdk) repo for build details.
