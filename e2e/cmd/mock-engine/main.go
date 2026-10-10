// Command mock-engine is a stand-in for llama-server on staging: it answers /health and
// streams a fixed answer for /v1/chat/completions, so the full path (Caddy, gateway, node)
// can be tested without a GPU. It never logs request bodies.
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const answer = "This is the AIs@home staging stub. No model ran; the path from the chat through " +
	"the gateway to a node works."

func main() {
	addr := flag.String("addr", ":8000", "listen address")
	delay := flag.Duration("delay", 20*time.Millisecond, "pause between streamed words")
	flag.Parse()

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		json.Unmarshal(body, &req)
		words := strings.Fields(answer)
		usage := map[string]any{"prompt_tokens": len(body) / 4, "completion_tokens": len(words)}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"object": "chat.completion", "usage": usage,
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": answer}}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		for i, word := range words {
			if i > 0 {
				word = " " + word
			}
			send(map[string]any{"object": "chat.completion.chunk",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": word}}}})
			select {
			case <-r.Context().Done():
				return
			case <-time.After(*delay):
			}
		}
		send(map[string]any{"object": "chat.completion.chunk",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		send(map[string]any{"object": "chat.completion.chunk", "choices": []any{}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	log.Printf("mock engine listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
