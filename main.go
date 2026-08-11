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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	aiBaseURL = "https://opencode.ai/zen/v1"
	aiModel   = "big-pickle"

	// Fallback anti-rate-limit (mesmo esquema do ipswap/text-resumer).
	proxyAttemptTimeout = 10 * time.Second
	proxyTestTimeout    = 6 * time.Second
	proxyPoolSize       = 40
	proxyBatchMax       = 200
	proxyMinWorking     = 40
)

var apiKey = os.Getenv("OPENCODE_API_KEY")

// proxySources são as mesmas fontes públicas usadas pelo ipswap.sh.
var proxySources = []string{
	"https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/http.txt",
	"https://raw.githubusercontent.com/ShiftyTR/Proxy-List/master/http.txt",
	"https://raw.githubusercontent.com/UptimerBot/proxy-list/main/http.txt",
	"https://raw.githubusercontent.com/MuRongDeHei/ProxyNode/main/http.txt",
	"https://raw.githubusercontent.com/vakhov/fresh-proxy-list/master/http.txt",
	"https://raw.githubusercontent.com/mertguvencli/http-proxy-list/main/proxy-list.txt",
	"https://raw.githubusercontent.com/Anonym0usWork1221/Free-Proxies/main/proxy.txt",
	"https://raw.githubusercontent.com/jetkai/proxy-list/main/online-proxies.txt",
	"https://raw.githubusercontent.com/roosterkid/openproxylist/main/HTTPS_RAW.txt",
}

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

	// Tenta direto (streaming). Se o zen responder 429 (rate limit), entra no
	// fallback de proxy que troca de IP automaticamente (igual o ipswap).
	if rateLimited := streamDirect(r.Context(), w, flusher, aiReq); !rateLimited {
		return
	}

	log.Printf("rate limit direto — rerodando via proxies")
	proxyBody := aiReq
	proxyBody.Stream = false

	out, err := chatViaProxy(r.Context(), proxyBody)
	if err != nil {
		fmt.Fprintf(w, "data: {\"error\":\"%v\"}\n\n", err)
		flusher.Flush()
		return
	}

	fmt.Fprintf(w, "data: %s\n\n", jsonEsc(out))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// streamDirect transmite a resposta do zen (SSE) para o cliente. Devolve
// true somente quando o zen responde 429 (rate limit) — nada é escrito e o
// chamador deve partir para o fallback de proxy.
func streamDirect(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, aiReq openAIRequest) bool {
	payload, _ := json.Marshal(aiReq)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, aiBaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(w, "data: {\"error\":\"API call failed: %v\"}\n\n", err)
		flusher.Flush()
		return false
	}
	setZenHeaders(httpReq)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintf(w, "data: {\"error\":\"API call failed: %v\"}\n\n", err)
		flusher.Flush()
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(w, "data: {\"error\":\"API status %d: %s\"}\n\n", resp.StatusCode, string(respBody))
		flusher.Flush()
		return false
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
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

// ── Fallback anti-rate-limit (estilo ipswap) ────────────────────────────────
//
// Quando o zen responde 429, o chat tenta a mesma pergunta ATRAVÉS de proxies
// até um responder (troca de IP sozinho). O fluxo é como o ipswap.sh:
//   1. pré-valida os candidatos contra https://opencode.ai (paralelo);
//   2. ordena os funcionais por latência e persiste no cache do ipswap
//      (working.txt/proxies.txt) para as próximas vezes saírem rápido;
//   3. tenta o request REAL em paralelo; o primeiro sucesso vence;
//   4. proxies que falham são descartados do cache e marcados como usados
//      (rotação — não reusa o mesmo IP na mesma rodada).

// proxyCandidate é um proxy já validado com a latência medida.
type proxyCandidate struct {
	url     *url.URL
	latency time.Duration
}

// chatViaProxy fica em loop buscando/validando proxies e tentando a chat
// completion até um responder — igual ao find_opencode_proxy do ipswap.
func chatViaProxy(ctx context.Context, body openAIRequest) (string, error) {
	used := map[string]bool{}
	attempted := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("rate limit persistiu após %d tentativas de proxy", attempted)
		}

		pool := nextPool(ctx, used)
		if len(pool) == 0 {
			log.Printf("sem proxies funcionais — aguardando e tentando de novo")
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("rate limit persistiu após %d tentativas de proxy", attempted)
			case <-time.After(2 * time.Second):
			}
			continue
		}

		winner, out, failed := tryProxyBatch(ctx, body, pool)
		attempted += len(pool)
		for _, p := range pool {
			used[p.url.String()] = true
		}
		for _, f := range failed {
			discardProxy(f)
		}
		if winner != nil {
			log.Printf("sucesso via proxy %s após %d tentativas", winner, attempted)
			saveWinnerProxy(winner)
			return out, nil
		}
		log.Printf("pool esgotado (%d proxies) — buscando mais", len(pool))
	}
}

// nextPool monta um pool de proxies VALIDADOS contra https://opencode.ai.
// Usa primeiro o cache do ipswap (proxy ativo do watchdog + já validados);
// se faltar, baixa as fontes públicas. Devolve os ainda não usados nesta
// rodada, ordenados por latência.
func nextPool(ctx context.Context, used map[string]bool) []proxyCandidate {
	cache := filterUsed(cacheCandidates(), used)
	validated := testProxies(ctx, cache)

	if len(validated) < proxyMinWorking {
		more := testProxies(ctx, filterUsed(sourceCandidates(), used))
		validated = append(validated, more...)
	}

	sort.Slice(validated, func(i, j int) bool { return validated[i].latency < validated[j].latency })
	if len(validated) > proxyBatchMax {
		validated = validated[:proxyBatchMax]
	}
	if len(validated) > 0 {
		saveWorking(validated)
	}
	return validated
}

// testProxies valida os candidatos contra https://opencode.ai EM PARALELO
// (como o test_proxies_batch do ipswap) e devolve os que responderam 200,
// ordenados por latência. A própria validação não gasta cota do zen.
func testProxies(ctx context.Context, candidates []*url.URL) []proxyCandidate {
	if len(candidates) == 0 {
		return nil
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		sem   = make(chan struct{}, proxyPoolSize)
		ok    []proxyCandidate
		start = time.Now()
	)
	for _, p := range candidates {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(p *url.URL) {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			if !testProxy(ctx, p) {
				return
			}
			mu.Lock()
			ok = append(ok, proxyCandidate{url: p, latency: time.Since(t0)})
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	sort.Slice(ok, func(i, j int) bool { return ok[i].latency < ok[j].latency })
	log.Printf("validação: %d candidatos, %d funcionais (%s)",
		len(candidates), len(ok), time.Since(start).Round(time.Millisecond))
	return ok
}

// testProxy faz um GET rápido em https://opencode.ai ATRAVÉS do proxy.
func testProxy(ctx context.Context, p *url.URL) bool {
	transport := &http.Transport{
		Proxy:               http.ProxyURL(p),
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	client := &http.Client{Timeout: proxyTestTimeout, Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://opencode.ai", nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	return err == nil && len(b) > 0
}

// callZen faz uma chat completion (não-streaming) pelo zen, direto ou via proxy.
// Para o fallback, o proxy com timeout curto: o loop troca de proxy rápido em
// vez de ficar preso num proxy morto.
func callZen(ctx context.Context, proxyURL *url.URL, body openAIRequest) (string, int, error) {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	timeout := 45 * time.Second
	if proxyURL != nil {
		timeout = proxyAttemptTimeout
	}
	client := &http.Client{Timeout: timeout, Transport: transport}

	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, aiBaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	setZenHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", resp.StatusCode, fmt.Errorf("rate limit")
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return string(data), resp.StatusCode, fmt.Errorf("zen retornou status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", resp.StatusCode, err
	}

	var zenResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &zenResp); err != nil {
		return "", resp.StatusCode, fmt.Errorf("falha ao decodificar resposta do zen: %v", err)
	}
	if len(zenResp.Choices) == 0 || strings.TrimSpace(zenResp.Choices[0].Message.Content) == "" {
		return "", resp.StatusCode, fmt.Errorf("zen retornou resposta vazia")
	}
	return strings.TrimSpace(zenResp.Choices[0].Message.Content), resp.StatusCode, nil
}

// tryProxyBatch tenta a chat completion REAL em paralelo (proxyPoolSize) pelos
// candidatos. O primeiro sucesso vence e cancela os demais. Devolve o proxy
// vencedor, a resposta e os proxies que falharam (para descartar do cache).
func tryProxyBatch(ctx context.Context, body openAIRequest, list []proxyCandidate) (*url.URL, string, []*url.URL) {
	if len(list) == 0 {
		return nil, "", nil
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		sem       = make(chan struct{}, proxyPoolSize)
		attempted int64
		failed    []*url.URL
		winnerURL *url.URL
		winnerOut string
		start     = time.Now()
	)
	for _, pc := range list {
		if child.Err() != nil {
			break
		}
		wg.Add(1)
		go func(p *url.URL) {
			defer wg.Done()
			if child.Err() != nil {
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			out, status, err := callZen(child, p, body)
			if child.Err() != nil {
				return
			}
			n := atomic.AddInt64(&attempted, 1)
			if err != nil {
				log.Printf("proxy %s falhou (%dª tentativa): %v", p, n, err)
				mu.Lock()
				failed = append(failed, p)
				mu.Unlock()
				return
			}
			if status != http.StatusOK {
				log.Printf("proxy %s retornou status %d — descartando", p, status)
				mu.Lock()
				failed = append(failed, p)
				mu.Unlock()
				return
			}
			mu.Lock()
			if winnerURL == nil {
				winnerURL = p
				winnerOut = out
				cancel()
			}
			mu.Unlock()
		}(pc.url)
	}
	wg.Wait()
	log.Printf("lote: %d candidatos, %d tentativas reais, vencedor=%v (%s)",
		len(list), atomic.LoadInt64(&attempted), winnerURL, time.Since(start).Round(time.Millisecond))
	return winnerURL, winnerOut, failed
}

// ── Cache do ipswap (compartilhado com o script ipswap.sh) ──────────────────

func ipswapCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "ipswap"), nil
}

// cacheCandidates junta os proxies do cache do ipswap: o proxy ativo do
// watchdog (proxy.env) + os já validados (winner/working/proxies).
func cacheCandidates() []*url.URL {
	var out []*url.URL
	seen := map[string]bool{}
	if p := activeProxy(); p != nil && !seen[p.String()] {
		seen[p.String()] = true
		out = append(out, p)
	}
	dir, err := ipswapCacheDir()
	if err != nil {
		return out
	}
	readProxyFile(filepath.Join(dir, "winner.txt"), true, seen, &out)
	readProxyFile(filepath.Join(dir, "working.txt"), true, seen, &out)
	readProxyFile(filepath.Join(dir, "proxies.txt"), true, seen, &out)
	return out
}

// sourceCandidates baixa as fontes públicas de proxies em paralelo.
func sourceCandidates() []*url.URL {
	var (
		out  []*url.URL
		seen = map[string]bool{}
		mu   sync.Mutex
		wg   sync.WaitGroup
	)
	for _, src := range proxySources {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			body, err := fetchURL(src)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, line := range strings.Split(string(body), "\n") {
				if len(out) >= proxyBatchMax {
					return
				}
				addProxy(line, true, seen, &out)
			}
		}(src)
	}
	wg.Wait()
	return out
}

// activeProxy lê o proxy que o ipswap mantém ativo AGORA
// (~/.config/ipswap/proxy.env) — o primeiro candidato a tentar.
func activeProxy() *url.URL {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "ipswap", "proxy.env"))
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		const prefix = "export HTTP_PROXY="
		if i := strings.Index(line, prefix); i >= 0 {
			v := strings.Trim(strings.TrimSpace(line[i+len(prefix):]), `"'`)
			if u, err := url.Parse(v); err == nil && u.Host != "" {
				return u
			}
		}
	}
	return nil
}

func fetchURL(raw string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "opencode-web-chat/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, os.ErrNotExist
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func readProxyFile(path string, withPrefix bool, seen map[string]bool, out *[]*url.URL) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		addProxy(line, withPrefix, seen, out)
	}
}

// saveWorking persiste os proxies validados no formato do ipswap:
// working.txt = "latencia_ms|url" (ordenado) e proxies.txt = "ts|url" (cache).
func saveWorking(working []proxyCandidate) {
	dir, err := ipswapCacheDir()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	var wb, cb strings.Builder
	for _, p := range working {
		ms := p.latency.Milliseconds()
		if ms < 1 {
			ms = 1
		}
		wb.WriteString(fmt.Sprintf("%d|%s\n", ms, p.url.String()))
		cb.WriteString(fmt.Sprintf("%d|%s\n", now, p.url.String()))
	}
	_ = os.WriteFile(filepath.Join(dir, "working.txt"), []byte(wb.String()), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "proxies.txt"), []byte(cb.String()), 0o600)
}

// saveWinnerProxy grava o proxy que conseguiu responder o chat. Na próxima
// chamada o cacheCandidates tenta ele primeiro, acelerando o fallback.
func saveWinnerProxy(p *url.URL) {
	if p == nil {
		return
	}
	dir, err := ipswapCacheDir()
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "winner.txt"), []byte(fmt.Sprintf("%d|%s\n", time.Now().Unix(), p.String())), 0o600)
}

// discardProxy remove um proxy que falhou do cache (working.txt/proxies.txt),
// igual ao discard_proxy do ipswap.
func discardProxy(p *url.URL) {
	if p == nil {
		return
	}
	dir, err := ipswapCacheDir()
	if err != nil {
		return
	}
	for _, name := range []string{"working.txt", "proxies.txt"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var out []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			u := line
			if i := strings.Index(u, "|"); i >= 0 {
				u = u[i+1:]
			}
			if u == p.String() {
				continue
			}
			out = append(out, line)
		}
		_ = os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
	}
}

// addProxy normaliza uma linha "host:porta" ou "meta|url" para http://host:porta.
func addProxy(line string, withPrefix bool, seen map[string]bool, out *[]*url.URL) {
	u := strings.TrimSpace(line)
	if u == "" || strings.HasPrefix(u, "#") {
		return
	}
	if withPrefix {
		if i := strings.Index(u, "|"); i >= 0 {
			u = u[i+1:]
		}
		u = strings.TrimSpace(u)
		if u == "" || strings.HasPrefix(u, "#") {
			return
		}
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	if seen[u] {
		return
	}
	if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
		seen[u] = true
		if out != nil {
			*out = append(*out, parsed)
		}
	}
}

func filterUsed(cands []*url.URL, used map[string]bool) []*url.URL {
	var out []*url.URL
	for _, u := range cands {
		if !used[u.String()] {
			out = append(out, u)
		}
	}
	return out
}

// effectiveAPIKey usa a chave da env se definida; caso contrário entra no modo
// anônimo gratuito do zen (chave placeholder "public", como o opencode CLI faz).
func effectiveAPIKey() string {
	if apiKey != "" {
		return apiKey
	}
	return "public"
}

// setZenHeaders monta os headers que o zen aceita no modo anônimo: sem o
// user-agent/runtime do cliente oficial, a chave "public" responde 429.
func setZenHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+effectiveAPIKey())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "opencode/1.18.14 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
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
