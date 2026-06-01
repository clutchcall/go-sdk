// Realtime frame-track example: publish robot telemetry and subscribe to it
// through the ClutchCall relay.
//
// Run (with the native engine on the path and a relay reachable):
//
//	CGO_LDFLAGS="-L/path/to/lib" LD_LIBRARY_PATH=/path/to/lib \
//	RELAY_URL=quic://relay.clutchcall.dev:4443 go run .
package main

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	clutchcall "github.com/clutchcall/clutchcall-sdk/go/pkg"
)

func main() {
	url := os.Getenv("RELAY_URL")
	if url == "" {
		url = "quic://127.0.0.1:4443"
	}
	const ns, name = "robot/turtlebot4-001", "odom"
	var recv, badPrio int64

	// The state callback reports Connecting/Connected/Reconnecting/Closed/Failed.
	subc, _ := clutchcall.ConnectMoqt(url, "", func(s int) { fmt.Println("sub state", s) })
	pubc, _ := clutchcall.ConnectMoqt(url, "", func(s int) { fmt.Println("pub state", s) })

	// Subscribe first: the relay holds it until the publisher announces.
	sub, _ := subc.SubscribeFrame(ns, name, func(ts uint64, priority uint8, data []byte) {
		atomic.AddInt64(&recv, 1)
		if priority != 200 {
			atomic.AddInt64(&badPrio, 1)
		}
	})
	track, _ := pubc.PublishFrame(ns, name, "ros.telemetry", "ros2/cdr", 128)

	for i := 0; i < 100; i++ {
		cdr := make([]byte, 48) // stand-in for a serialized message
		track.Write(uint64(i*1000), cdr, 200)
		time.Sleep(100 * time.Millisecond) // 10 Hz
	}
	time.Sleep(time.Second)
	fmt.Printf("received %d frames; priority ok: %v\n", atomic.LoadInt64(&recv), atomic.LoadInt64(&badPrio) == 0)

	track.Close()
	sub.Close()
	pubc.Close()
	subc.Close()
}
