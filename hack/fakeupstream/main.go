// Command fakeupstream is a stand-in for ai-gateway.vercel.sh, so a
// pi-durable turn can run against stage B's egress gateway without a
// network call or an API key. It serves TLS with a self-signed leaf for
// the hostname named on the command line, answers the two shapes
// pi-ai's gateway provider and gate.sh's proxy-check both use (the
// Anthropic Messages API at /v1/messages, and the OpenAI-compatible
// /v1/chat/completions), and returns 401 unless x-api-key matches
// -api-key exactly, the same replace-never-add contract egress.go
// documents.
//
// It never runs a real coding agent. A turn settles because the fake
// model gives a plain final answer with no tool call, except for one
// case this binary recognizes on purpose: a prompt of the form "Run
// this exact command with the bash tool and reply with its output
// only: <cmd>". For that one it issues a tool_use for <cmd>, then
// echoes the tool_result's own text back as the final answer, so the
// network-refusal checks in gate.sh still run <cmd> for real inside
// the actor's own sandbox and report what actually happened.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address to listen on (TLS)")
	hostname := flag.String("hostname", "ai-gateway.vercel.sh", "hostname the leaf certificate covers")
	apiKey := flag.String("api-key", "test-fake-key", "x-api-key value that gets a 200 instead of a 401")
	certOut := flag.String("cert-out", "", "write the self-signed CA's PEM here (defaults to a temp file)")
	readyFile := flag.String("ready-file", "", "write 'addr capath' here once listening, for a caller to poll instead of scraping stdout")
	delay := flag.Duration("delay", 0, "hold every /v1/messages reply this long before answering (G6: a slow outbound call the actor's own handler is waiting on)")
	flag.Parse()

	cert, caPEM, err := selfSignedLeaf(*hostname)
	if err != nil {
		log.Fatalf("fakeupstream: generating certificate: %v", err)
	}
	capath := *certOut
	if capath == "" {
		f, err := os.CreateTemp("", "fakeupstream-ca-*.pem")
		if err != nil {
			log.Fatalf("fakeupstream: temp ca file: %v", err)
		}
		capath = f.Name()
		f.Close()
	}
	if err := os.WriteFile(capath, caPEM, 0o644); err != nil {
		log.Fatalf("fakeupstream: writing %s: %v", capath, err)
	}

	ln, err := tls.Listen("tcp", *listen, &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		log.Fatalf("fakeupstream: listen %s: %v", *listen, err)
	}
	addr := ln.Addr().String()
	fmt.Printf("LISTEN %s\nCERT %s\n", addr, capath)
	if *readyFile != "" {
		if err := os.WriteFile(*readyFile, []byte(addr+" "+capath+"\n"), 0o644); err != nil {
			log.Fatalf("fakeupstream: writing %s: %v", *readyFile, err)
		}
	}

	h := &handler{apiKey: *apiKey, delay: *delay}
	log.Fatal(http.Serve(ln, h))
}

type handler struct {
	apiKey string
	delay  time.Duration
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("x-api-key"); got != h.apiKey {
		http.Error(w, `{"error":{"type":"authentication_error","message":"missing or wrong x-api-key"}}`, http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/v1/messages":
		h.messages(w, r)
	case "/v1/chat/completions":
		// gate.sh's proxy-check turn only ever reaches here with no
		// x-api-key (checking the proxy never adds one it wasn't
		// handed); the key check above already answered that case, so
		// a request that does carry the right key just gets a minimal
		// 200.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-fake", "choices": []any{
			map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"},
		}})
	default:
		http.NotFound(w, r)
	}
}

// runCommandPrompt matches gate.sh's exact wording for the two
// network-refusal turns and the proxy-check turn, each of which asks
// the model to run one literal command and report only its output.
var runCommandPrompt = regexp.MustCompile(`(?s)Run this exact command with the bash tool and reply with its output only: (.+)$`)

type messagesRequest struct {
	Model    string          `json:"model"`
	Messages []anthropicTurn `json:"messages"`
}

type anthropicTurn struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     any             `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// messages always answers as a stream: pi-ai's @anthropic-ai/sdk client
// asks for one (confirmed by running this against a real pi-durable
// turn, which failed with "Anthropic stream ended without a stop
// reason" against a plain-JSON reply), so a non-streaming body is not
// enough even though the Anthropic API itself accepts either shape.
func (h *handler) messages(w http.ResponseWriter, r *http.Request) {
	var req messagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.delay > 0 {
		time.Sleep(h.delay)
	}
	text, blocks := replyFor(req)

	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant", "model": req.Model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
	}})

	stopReason := "end_turn"
	if len(blocks) > 0 {
		stopReason = "tool_use"
		b := blocks[0]
		send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": map[string]any{}}})
		inputJSON, _ := json.Marshal(b.Input)
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(inputJSON)}})
	} else {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text}})
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 1}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// replyFor decides this turn's reply without running a real model:
//   - the last message is a fresh user prompt matching runCommandPrompt:
//     issue a tool_use for the command inside it.
//   - the last message is a user turn carrying that tool's result:
//     echo the result's own text back, since the prompt asked for the
//     output and nothing else.
//   - anything else: a plain "done" with no tool call, enough for the
//     harness to settle; gate.sh never asserts the content of these
//     coding turns, only that they settle.
func replyFor(req messagesRequest) (text string, blocks []contentBlock) {
	if len(req.Messages) == 0 {
		return "done", nil
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		return "done", nil
	}
	var parts []contentBlock
	if err := json.Unmarshal(last.Content, &parts); err != nil {
		// Plain-string content, not a content-block array.
		var s string
		json.Unmarshal(last.Content, &s)
		parts = []contentBlock{{Type: "text", Text: s}}
	}
	for _, p := range parts {
		if p.Type == "tool_result" {
			var resultParts []contentBlock
			if err := json.Unmarshal(p.Content, &resultParts); err == nil && len(resultParts) > 0 {
				return resultParts[0].Text, nil
			}
			var s string
			json.Unmarshal(p.Content, &s)
			return s, nil
		}
	}
	for _, p := range parts {
		if p.Type != "text" {
			continue
		}
		if m := runCommandPrompt.FindStringSubmatch(p.Text); m != nil {
			return "", []contentBlock{{Type: "tool_use", ID: "toolu_fake1", Name: "bash", Input: map[string]any{"command": strings.TrimSpace(m[1])}}}
		}
	}
	return "done", nil
}

func selfSignedLeaf(hostname string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		IsCA:         true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, caPEM, nil
}
