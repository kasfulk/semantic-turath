# Semantic Turath (Go)

Cari nukilan perkataan ulama dari pertanyaan bebas Bahasa Indonesia.
Rewrite Go dari `semantic_turath.py` — perilaku setara, tanpa dependensi eksternal (stdlib saja), prompt LLM disalin verbatim.

## Pipeline (5 tahap)

1. `keywords` — 4 model `kn/*` paralel; masing-masing 4+3+2 keyword Arab (min 2/3/4 kalimat)
2. `merge` — `agnes/agnes-3.0-flash` → 1 array unik, maks 30
3. `verify` — `mercury-decide` (type=noul) → skor relevansi 0..1; ambang 0.5, fallback top-5
4. `search` — ES `konten_kitab` + api.turath.io + backend.ketabonline.com (paralel, 8 hasil/sumber/keyword)
5. `curate` — `agnes/agnes-3.0-flash`, batch 20 nukilan/panggilan, cap 60 → buang tak relevan + terjemahan Indonesia

## Build

```bash
go build -o semantic-turath .
```

## CLI

```bash
./semantic-turath "dalil sholat 5 waktu" --limit 20 --out hasil.json
./semantic-turath "hukum makan keong" --limit 5
./semantic-turath "..." --stages keywords,merge     # debug bertahap
./semantic-turath --serve 20052                     # HTTP server (bind 0.0.0.0:20052)
./semantic-turath --print-config                    # cetak env efektif (key di-mask)
./semantic-turath --version
```

Urutan argumen bebas: `"pertanyaan" --limit 5` dan `--limit 5 "pertanyaan"` sama-sama bekerja.

## Konfigurasi (env)

Tanpa dependensi: `.env` dibaca sendiri saat start (env yang sudah ada menang).

```bash
cp .env.example .env   # lalu isi ST_KEY
```

| Env | Default | Keterangan |
|---|---|---|
| `ST_GW` | `http://localhost:20128/v1` | gateway LLM |
| `ST_KEY` | **wajib** (tidak ada default) | bearer token gateway; isi via `.env` atau env |
| `ST_ES` | `http://localhost:9200/konten_kitab/_search` | Elasticsearch |
| `PORT` | — | dipakai sebagai port server bila `--serve` tidak diberikan |

`.env` tidak di-commit (`.gitignore`). Cek wiring dengan `./semantic-turath --print-config`.

Catatan: pakai `localhost` untuk gateway (host ini me-resolve ke IPv6 `::1`; `127.0.0.1` tidak nyambung).

## HTTP API (default port 20052)

```bash
curl localhost:20052/health
curl "localhost:20052/ask?q=hukum%20makan%20keong&limit=3"
```

- `q` wajib; kosong → HTTP 400 `{"error":"parameter q wajib"}`
- `limit` default 20, dipotong ke rentang 1..50
- path lain → HTTP 404 `{"error":"pakai /ask?q=<pertanyaan>&limit=20"}`
- CORS: `Access-Control-Allow-Origin: *`
- run dingin ±5 menit; pertanyaan identik dilayani dari cache

## Swagger

- UI (RapiDoc, embedded): http://localhost:20052/swagger/
- OpenAPI 3.0: http://localhost:20052/swagger/openapi.yaml

## Deploy PM2

```bash
pm2 start ./semantic-turath --name semantic-turath -- --serve 20052
pm2 save
pm2 status
```

atau: `pm2 start ecosystem.config.cjs`

## Cache

`~/.cache/semantic-turath/<sha1(q+"|"+stages)[:16]>.json` — kunci TIDAK memuat `limit`;
kurasi selalu menyimpan minimal 20 hasil sehingga permintaan limit kecil dilayani dari cache (< 1 detik).

## Konfigurasi (env)

| Env | Default |
|---|---|
| `ST_GW` | `http://localhost:20128/v1` |
| `ST_KEY` | wajib diisi (via `.env` atau env) |
| `ST_ES` | `http://localhost:9200/konten_kitab/_search` |
| `PORT` | `20052` |

## Struktur

```
main.go        driver pipeline + cache + CLI
stages.go      5 tahap pipeline + prompt verbatim
llm.go         gateway LLM (trailer [DONE], fallback reasoning), ES/HTTP helpers
server.go      /health /ask /swagger/ + CORS, UI embedded
openapi.yaml   OpenAPI 3.0
static/        RapiDoc JS (embedded)
```
