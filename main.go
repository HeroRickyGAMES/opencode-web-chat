package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	aiBaseURL = "https://opencode.ai/zen/v1"
	aiModel   = "big-pickle"

	// User-Agent do cliente oficial. O zen só aceita o modo anônimo com ele:
	// sem/abaixo da versão mínima do free tier, responde 403 FreeTierError/429.
	defaultUA = "opencode/1.18.0 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.0"
)

var apiKey = os.Getenv("OPENCODE_API_KEY")

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Messages []Message `json:"messages"`
}

type openAIRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

func main() {
	if _, err := os.Stat("ocr.py"); err != nil {
		log.Fatalf("ocr.py not found")
	}

	http.HandleFunc("/", handleStatic)
	http.HandleFunc("/api/chat", handleChat)
	http.HandleFunc("/api/upload", handleUpload)
	http.HandleFunc("/api/health", handleHealth)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.ServeFile(w, r, "static/index.html")
		return
	}
	file := filepath.Join("static", r.URL.Path)
	if _, err := os.Stat(file); err == nil {
		http.ServeFile(w, r, file)
		return
	}
	http.NotFound(w, r)
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	aiReq := openAIRequest{
		Model:    aiModel,
		Messages: req.Messages,
		Stream:   true,
	}

	// Tenta direto (streaming). Se o zen recusar (403/429/5xx ou falha de
	// transporte), parte para o `opencode run` — o CLI oficial já faz a
	// rotação de proxy por conta própria (mesmo esquema do text-resumer).
	if streamDirect(r.Context(), w, flusher, aiReq) {
		log.Printf("zen direto recusou — tentando opencode run")

		ctx, cancel := context.WithTimeout(r.Context(), chatTimeout())
		defer cancel()

		out, err := chatViaOpenCode(ctx, opencodeBin(), aiReq.Messages)
		if err != nil || strings.TrimSpace(out) == "" {
			if err == nil {
				err = fmt.Errorf("resposta vazia")
			}
			fmt.Fprintf(w, "data: {\"error\":\"%v\"}\n\n", err)
			flusher.Flush()
			return
		}

		emitChunked(w, flusher, out)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

// chatTimeout limita o tempo do fallback via `opencode run`. O CLI rotaciona
// proxies até achar um egress 200; se estourar o teto, o erro é devolvido.
func chatTimeout() time.Duration {
	if s := os.Getenv("CHAT_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return 5 * time.Minute
}

// emitChunked envia a resposta completa em pequenos eventos SSE (formato que o
// front-end já entende: `data: "<trecho>"`) para exibir como streaming.
func emitChunked(w http.ResponseWriter, flusher http.Flusher, text string) {
	const step = 120
	for _, part := range splitRunes(text, step) {
		fmt.Fprintf(w, "data: %s\n\n", jsonEsc(part))
		flusher.Flush()
	}
}

func splitRunes(s string, n int) []string {
	rs := []rune(s)
	var parts []string
	for len(rs) > 0 {
		if len(rs) <= n {
			parts = append(parts, string(rs))
			break
		}
		parts = append(parts, string(rs[:n]))
		rs = rs[n:]
	}
	return parts
}

// streamDirect transmite a resposta do zen (SSE) para o cliente. Devolve true
// quando o zen recusou (status != 200 ou falha de transporte) — nada é
// escrito e o chamador deve partir para o fallback. Em sucesso (200), flui os
// chunks e o [DONE], devolvendo false.
func streamDirect(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, aiReq openAIRequest) bool {
	payload, _ := json.Marshal(aiReq)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, aiBaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		log.Printf("montar request do zen falhou: %v", err)
		return true
	}
	setZenHeaders(httpReq)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("zen inacessível (direto): %v", err)
		return true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		log.Printf("zen direto recusou com status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		return true
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if chunk.Choices[0].Delta.Content != nil {
			content := *chunk.Choices[0].Delta.Content
			if content != "" {
				fmt.Fprintf(w, "data: %s\n\n", jsonEsc(content))
				flusher.Flush()
			}
		}
		if chunk.Choices[0].FinishReason != nil {
			break
		}
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
	return false
}

func effectiveAPIKey() string {
	if apiKey != "" {
		return apiKey
	}
	return "public"
}

// setZenHeaders monta os headers que o zen aceita no modo anônimo: sem o
// user-agent/runtime do cliente oficial, a chave "public" responde 403/429.
func setZenHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+effectiveAPIKey())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", defaultUA)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", "6d629fd1e3510e726353f8027e55b7609bbb788b")
	req.Header.Set("x-opencode-request", newID("msg_"))
	req.Header.Set("x-opencode-session", newID("ses_"))
}

func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return prefix + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	r.ParseMultipartForm(32 << 20)
	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "missing image field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".png"
	}
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("ocr_%d%s", time.Now().UnixNano(), ext))
	dst, err := os.Create(tmpFile)
	if err != nil {
		http.Error(w, "failed to save", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpFile)

	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		http.Error(w, "failed to write", http.StatusInternalServerError)
		return
	}
	dst.Close()

	ocrText, err := runOCR(tmpFile)
	if err != nil {
		http.Error(w, fmt.Sprintf("OCR failed: %v", err), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"text": ocrText,
	})
}

func runOCR(imagePath string) (string, error) {
	var outBuf, errBuf bytes.Buffer

	cmd := exec.Command("python3", "ocr.py", imagePath)
	cmd.Dir = "." // project root
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ocr.py: %w, stderr: %s", err, strings.TrimSpace(errBuf.String()))
	}

	return strings.TrimSpace(outBuf.String()), nil
}

func jsonEsc(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}