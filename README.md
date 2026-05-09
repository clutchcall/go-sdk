# ClutchCall Go SDK The official Go wrapper for the ClutchCall telephony system. This SDK includes the natively generated Protobuf definitions and the high-level `client.go` multiplexer. ## Installation This SDK is a standard Go module. ```bash
go get github.com/clutchcall/go-sdk
``` ## Quick Start Ensure `CLUTCHCALL_CREDENTIALS` is set in your environment variables. ```go
package main import ( "log" "github.com/clutchcall/go-sdk/pkg"
) func main() { // Automatically binds via zero-trust JWT Auth client, err := pkg.NewClutchCallClient("pbx.clutchcall.com:443") if err != nil { log.Fatalf("Failed to connect: %v", err) } // Dial out to a real device! resp, err := client.Originate("+1234567890", "wss://my-chatbot.com/media", "") // Connect to Media WebSockets stream := pkg.NewClutchCallAudioStream() stream.Connect("wss://pbx.clutchcall.com/media/session_789") stream.OnAudio(func(pcm []byte) { // Direct integration with LLMs! }) stream.ReceiveAudioLoop()
}
```
