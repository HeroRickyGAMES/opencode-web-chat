package main

import (
	"bytes"
	"context"
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

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Messages []Message `json:"messages"`
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

	// Tudo passa pelo `opencode run`: o CLI oficial já lida com a rotação de
	// egress/proxy por conta própria (mesmo esquema do text-resumer), então o
	// zen direto (que vive em 403/429) foi eliminado.
	bin := opencodeBin()
	if bin == "" {
		fmt.Fprintf(w, "data: {\"error\":\"opencode CLI nao encontrado (OPENCODE_BIN ou PATH)\"}\n\n")
		flusher.Flush()
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), chatTimeout())
	defer cancel()

	out, err := chatViaOpenCode(ctx, bin, req.Messages)
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