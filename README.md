# ClutchCall Go SDK

The official Go wrapper for ClutchCall — telephony origination, media streaming,
and zero-trust JWT auth over ALPN-QUIC.

## Installation

```bash
go get github.com/clutchcall/go-sdk
```

## Quick start

Set `CLUTCHCALL_CREDENTIALS` to your service-account JSON, then:

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
