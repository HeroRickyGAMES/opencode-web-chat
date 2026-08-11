# opencode-web-chat

Chat web com o modelo **Big Pickle** do **OpenCode Zen** (`https://opencode.ai/zen/v1/chat/completions`), usando a **API gratuita anônima** (chave placeholder `public` + headers do cliente oficial) — sem precisar de conta nem chave.

Inclui **OCR de imagens** (tesseract) e **anti-rate-limit automático**: quando o Zen responde `429`, o servidor troca de IP sozinho via proxies (mesmo esquema do [ipswap](https://github.com/HeroRickyGAMES/opencode-changer) / text-resumer).

## Funcionalidades

- 💬 Chat com streaming (SSE) — resposta aparece em tempo real
- 📷 Upload de imagem com **OCR** (tesseract `por+eng`); o texto extraído vira contexto para a IA
- 🔊 Botão "Ouvir" em cada resposta (TTS do navegador, pt-BR)
- 🖼️ Arrastar e soltar imagem na página
- 🔄 **Anti-rate-limit**: no `429`, pré-valida proxies contra `opencode.ai` e tenta a mesma pergunta até um responder

## Como funciona

1. O servidor monta um `POST` para o Zen com `model: "big-pickle"`, `Authorization: Bearer public` e os headers do cliente opencode (`User-Agent`, `x-opencode-client`, `x-opencode-request/session`, etc.) — o modo anônimo que o próprio opencode CLI usa.
2. **Sem rate limit** → a resposta é transmitida direto ao navegador via SSE.
3. **Com rate limit (429)** → ativa o fallback de proxy (igual ao ipswap):
   - pré-valida os proxies contra `https://opencode.ai` em paralelo e ordena por latência;
   - usa primeiro o cache do ipswap (`~/.cache/ipswap/` + proxy ativo em `~/.config/ipswap/proxy.env`);
   - se faltar, baixa as fontes públicas de proxies;
   - tenta o request real em paralelo; o primeiro sucesso vence e é salvo em `winner.txt`;
   - proxies que falham são descartados do cache e não são reusados na mesma rodada.
   - A resposta chega completa (um único chunk SSE) — o navegador exibe igual.

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
- Python 3 + [tesseract](https://github.com/tesseract-ocr/tesseract) para o OCR (instalado em `/tmp/tesseract-extract/usr/bin/tesseract` com `por+eng` — veja `ocr.py`)

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
> `rate limit direto — rerodando via proxies` e o fluxo de validação de proxies
> (`validação: N candidatos, M funcionais`). Em caso de proxy vencedor:
> `sucesso via proxy http://... após N tentativas`.

## Segurança

- **Nenhuma credencial é embutida no código.** A chave anônima `public` não é um segredo.
- `OPENCODE_API_KEY` (se usada) deve ser configurada via variável de ambiente do serviço, nunca no código.
- O binário compilado `chat-server` fica de fora do repositório (`.gitignore`).

## Projetos relacionados

- [text-resumer](https://github.com/HeroRickyGAMES/text-resumer) — API Go que resume textos pelo mesmo Zen anônimo, com fallback de proxy
- [opencode-changer](https://github.com/HeroRickyGAMES/opencode-changer) — ipswap: troca de IP automática para o CLI do opencode
