// Command mock-upstream is a fake OpenAI-compatible server for local demos
// and load tests. It answers every chat completion with a canned reply and
// can be told to fail a fraction of requests with 503.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":9001", "listen address")
	name := flag.String("name", "mock", "provider name echoed in responses")
	fail := flag.Float64("fail", 0, "fraction of requests to fail with 503 (0..1)")
	delay := flag.Duration("delay", 0, "artificial latency per request")
	flag.Parse()

	http.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(*delay)
		if rand.Float64() < *fail {
			log.Printf("%s: 503 (injected failure)", *name)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"injected failure","type":"server_error"}}`)
			return
		}

		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":{"message":"invalid JSON","type":"invalid_request_error"}}`, http.StatusBadRequest)
			return
		}
		prompt := ""
		if n := len(req.Messages); n > 0 {
			prompt = req.Messages[n-1].Content
		}
		reply := fmt.Sprintf("Hello from %s! You said: %q", *name, prompt)
		log.Printf("%s: 200 model=%s stream=%v", *name, req.Model, req.Stream)

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			f, _ := w.(http.Flusher)
			for i, word := range strings.Fields(reply) {
				if i > 0 {
					word = " " + word
				}
				chunk, _ := json.Marshal(map[string]any{
					"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": req.Model,
					"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": word}}},
				})
				fmt.Fprintf(w, "data: %s\n\n", chunk)
				f.Flush()
				time.Sleep(40 * time.Millisecond)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": reply}}},
			"usage": map[string]int{"prompt_tokens": 9, "completion_tokens": 12, "total_tokens": 21},
		})
	})

	log.Printf("%s listening on %s (fail=%.0f%%, delay=%s)", *name, *addr, *fail*100, *delay)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
