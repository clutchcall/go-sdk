package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	clutchcall "github.com/clutchcall/go-sdk/pkg"
	"github.com/gorilla/websocket"
)

func main() {
	openApiKey := os.Getenv("OPENAI_API_KEY")
	if openApiKey == "" {
		openApiKey = "your-openai-api-key"
	}

	credentialsPath := os.Getenv("CLUTCHCALL_CREDENTIALS")
	if credentialsPath == "" {
		credentialsPath = "service_account.json"
	}

	client, err := clutchcall.NewClient("quic://127.0.0.1:9090", credentialsPath)
	if err != nil {
		log.Fatalf("Failed to initialize SDK cleanly: %v", err)
	}

	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+openApiKey)
	headers.Add("OpenAI-Beta", "realtime=v1")
	aiConn, _, err := websocket.DefaultDialer.Dial("wss://api.openai.com/v1/realtime?model=gpt-4o-realtime-preview-2024-10-01", headers)
	if err != nil {
		log.Fatalf("Failed to connect OpenAI: %v", err)
	}
	defer aiConn.Close()

	sessionUpdate := map[string]interface{}{
		"type": "session.update",
		"session": map[string]interface{}{
			"modalities":          []string{"audio", "text"},
			"instructions":        "You are a highly efficient telecom agent answering questions.",
			"voice":               "alloy",
			"input_audio_format":  "pcm16",
			"output_audio_format": "pcm16",
		},
	}
	aiConn.WriteJSON(sessionUpdate)

	callSid := "simulated_call_id"
	err = client.Dial("+15550000000", "trunk_ai_test", "", 60000)
	if err != nil {
		log.Fatalf("Dial err natively: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for {
			header := make([]byte, 8)
			time.Sleep(100 * time.Millisecond)

			if len(header) < 8 {
				continue
			}

			payloadLen := binary.LittleEndian.Uint32(header[0:4])
			methodId := binary.LittleEndian.Uint32(header[4:8])
			payloadBytes := make([]byte, payloadLen)

			if methodId == uint32(clutchcall.MethodAudioFrame) {
				_, pcmRaw := client.DeserializeAudioFrame(payloadBytes)
				b64Audio := base64.StdEncoding.EncodeToString(pcmRaw)

				msg := map[string]interface{}{
					"type":  "input_audio_buffer.append",
					"audio": b64Audio,
				}
				aiConn.WriteJSON(msg)
			} else if methodId == uint32(clutchcall.MethodStreamEvents) {
				sid, status := client.DeserializeCallEvent(payloadBytes)
				fmt.Printf("[Call Event] %s -> %s\n", sid, status)
			}
		}
	}()

	go func() {
		defer wg.Done()
		seq := uint64(0)
		for {
			_, message, err := aiConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("error : %v", err)
				}
				break
			}
			var event map[string]interface{}
			json.Unmarshal(message, &event)
			if event["type"] == "response.audio.delta" {
				deltaB64 := event["delta"].(string)
				pcmRaw, _ := base64.StdEncoding.DecodeString(deltaB64)
				fmt.Printf("Received %d bytes delta .\n", len(pcmRaw))

				payloadChunk := client.SerializeAudioFrame(callSid, pcmRaw, "PCMU", seq, false)
				seq++
				_ = payloadChunk
			}
		}
	}()

	wg.Wait()
}
