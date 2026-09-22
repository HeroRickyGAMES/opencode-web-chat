# opencode-web-chat

Chat web com o modelo **Big Pickle** do **OpenCode Zen** (`https://opencode.ai/zen/v1/chat/completions`), usando a **API gratuita anônima** (chave placeholder `public` + headers do cliente oficial) — sem precisar de conta nem chave.

Inclui **OCR de imagens** (tesseract) e **anti-rate-limit automático**: quando o Zen recusa a chamada direta (`403` FreeTierError / `429` / 5xx), o servidor reexecuta a pergunta via `opencode run` — o CLI oficial já faz a rotação de proxy/egress por conta própria (mesmo esquema do [text-resumer](https://github.com/HeroRickyGAMES/text-resumer)).

## Funcionalidades

- 💬 Chat com streaming (SSE) — resposta aparece em tempo real
- 📷 Upload de imagem com **OCR** (tesseract `por+eng`); o texto extraído vira contexto para a IA
- 🔊 Botão "Ouvir" em cada resposta (TTS do navegador, pt-BR)
- 🖼️ Arrastar e soltar imagem na página
- 🔄 **Anti-rate-limit**: direto ao Zen, com fallback automático via `opencode run` (que rotaciona o IP sozinho)

## Como funciona

1. O servidor monta um `POST` para o Zen com `model: "big-pickle"`, `Authorization: Bearer public` e os headers do cliente opencode (`User-Agent`, `x-opencode-client`, `x-opencode-request/session`, etc.) — o modo anônimo que o próprio opencode CLI usa.
2. **Sem rate limit** → a resposta é transmitida direto ao navegador via SSE.
3. **Com rate limit (`403`/`429`/5xx ou falha de transporte)** → o servidor roda a mesma pergunta via `opencode run` (agente built-in, não-interativo). O CLI oficial já lida com a rotação de egress/proxy internamente e devolve a resposta completa — enviada ao navegador em chunks SSE.

## Endpoints

| Método | Rota | Descrição |
|---|---|---|
| `GET` | `/` | Página do chat (`static/index.html`) |
| `POST` | `/api/chat` | Chat streaming (SSE). Body: `{"messages":[{"role":"user","content":"..."}]}` |
| `POST` | `/api/upload` | Upload de imagem (multipart `image`) → retorna `{"text":"<OCR>"}` |
| `GET` | `/api/health` | Healthcheck `{"status":"ok"}` |

## Como rodar

### Requisitos

- Go 1.22+ (ou apenas o binário compilado `chat-server`)
- **opencode CLI** (`~/.opencode/bin/opencode` ou no `PATH`) — usado como fallback anti-rate-limit; variavel `OPENCODE_BIN` para apontar outro caminho
- Python 3 + [tesseract](https://github.com/tesseract-ocr/tesseract) para o OCR (`/usr/bin/tesseract` com `por+eng` — veja `ocr.py`)

### Compilar e rodar

```bash
cd opencode-web-chat
go build -o chat-server .
PORT=8080 ./chat-server
```

Abrir: `http://localhost:8080`

> O OCR depende do caminho absoluto do tesseract em `ocr.py`. Ajuste `TESSERACT`/`TESSDATA` se estiver instalado em outro local.

## Variáveis de ambiente

| Variável | Padrão | Descrição |
|---|---|---|
| `PORT` | `8080` | Porta HTTP |
| `CHAT_TIMEOUT` | `5m` | Teto do fallback via `opencode run` |
| `OPENCODE_API_KEY` | *(vazio)* | Chave própria opcional. Vazio = modo anônimo gratuito do Zen (chave `public`) |

## Instalar como serviço (systemd)

```bash
sudo tee /etc/systemd/system/opencode-web-chat.service > /dev/null <<'EOF'
[Unit]
Description=OpenCode Web Chat (Big Pickle via OpenCode Zen)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=heroricky
WorkingDirectory=/home/heroricky/Server/share/steam_t/Projetos/opencode-web-chat
ExecStart=/home/heroricky/Server/share/steam_t/Projetos/opencode-web-chat/chat-server
Environment=PORT=8080
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now opencode-web-chat
```

Logs: `sudo journalctl -u opencode-web-chat -f`

## Testar manualmente

```bash
curl http://localhost:8080/api/health
# {"status":"ok"}

curl -N http://localhost:8080/api/chat \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"Responda apenas: ok"}]}'
# data: "ok"
# data: [DONE]
```

> Quando o modo direto estiver com rate limit, o log do servidor mostra
> `zen direto recusou com status 403/429...` e `zen direto recusou — tentando
> opencode run`. O `opencode run` roda em background, rotaciona o egress e
> devolve a resposta quando o proxy/IP certo responder.

## Segurança

- **Nenhuma credencial é embutida no código.** A chave anônima `public` não é um segredo.
- `OPENCODE_API_KEY` (se usada) deve ser configurada via variável de ambiente do serviço, nunca no código.
- O binário compilado `chat-server` fica de fora do repositório (`.gitignore`).

## Projetos relacionados

- [opencode-changer](https://github.com/HeroRickyGAMES/opencode-changer) — ipswap: troca de IP automática para o CLI do opencode
