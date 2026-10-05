# Semantic Turath (Go)

Cari nukilan perkataan ulama dari pertanyaan bebas Bahasa Indonesia.
Rewrite Go dari `semantic_turath.py` — perilaku setara (stdlib + `modernc.org/sqlite` + `golang.org/x/text`), prompt LLM disalin verbatim.

## Pipeline (6 tahap)

1. `keywords` — 4 model `kn/*` paralel; masing-masing 4+3+2 keyword Arab (min 2/3/4 kalimat)
2. `merge` — `agnes/agnes-3.0-flash` → 1 array unik, maks 30
3. `verify` — `mercury-decide` (type=noul) → skor relevansi 0..1; ambang 0.5, fallback top-5
4. `search` — ES `konten_kitab` (HTTP) + Turath (MCP stdio) + backend.ketabonline.com (HTTP) paralel, 8 hasil/sumber/keyword
5. `curate` — `agnes/agnes-3.0-flash`, batch 20 nukilan/panggilan, cap 60 → buang tak relevan + urutkan (tanpa terjemahan)
6. `translate` — `kn-b/deepseek-v4-1-flash`, batch 10 nukilan yang akan ditampilkan → `terjemahan` Indonesia

Verifikasi halaman Turath (`turath_get_page` via MCP) dijalankan antara curate dan translate: maks 12 nukilan Turath dicek sekuensial (teks halaman dinormalisasi: buang tag HTML, harakat/tatwil), gagal → dibuang, backfill satu putaran. Kalau MCP gagal, Turath jatuh ke HTTP langsung (perilaku lama) dan verifikasi dilewati.

## Build

```bash
go build -o semantic-turath .
```

## CLI

```bash
./semantic-turath "dalil sholat 5 waktu" --limit 20 --out hasil.json   # sinkron, tanpa queue
./semantic-turath "hukum makan keong" --limit 5
./semantic-turath "..." --stages keywords,merge     # debug bertahap
./semantic-turath --serve 20052                     # HTTP server (bind 0.0.0.0:20052)
./semantic-turath --request req_0123456789ab        # cetak laporan tersimpan (exit 1 bila tidak ada)
./semantic-turath --requests 10                     # riwayat ringkas (tanpa angka = 20)
./semantic-turath --print-config                    # cetak env efektif (key di-mask)
./semantic-turath --version
```

Urutan argumen bebas: `"pertanyaan" --limit 5` dan `--limit 5 "pertanyaan"` sama-sama bekerja.

## Konfigurasi (env)

`.env` dibaca sendiri saat start (env yang sudah ada menang).

```bash
cp .env.example .env   # lalu isi ST_KEY
```

| Env | Default | Keterangan |
|---|---|---|
| `ST_GW` | `http://localhost:20128/v1` | gateway LLM |
| `ST_KEY` | **wajib** (tidak ada default) | bearer token gateway; isi via `.env` atau env |
| `ST_ES` | `http://localhost:9200/konten_kitab/_search` | Elasticsearch |
| `ST_DB` | `~/.local/share/semantic-turath/requests.db` | SQLite request store |
| `ST_WORKERS` | `1` | worker pipeline (pipeline berat, jangan dinaikkan tanpa alasan) |
| `ST_TURATH_MCP` | `npx --yes tsx /home/kasjful-kurniawan/turath-mcp/src/index.ts` | perintah server MCP Turath |
| `PORT` | — | dipakai sebagai port server bila `--serve` tidak diberikan |

`.env` tidak di-commit (`.gitignore`). Cek wiring dengan `./semantic-turath --print-config`.

Catatan: pakai `localhost` untuk gateway (host ini me-resolve ke IPv6 `::1`; `127.0.0.1` tidak nyambung).

## HTTP API (default port 20052)

```bash
curl localhost:20052/health
curl -X POST localhost:20052/ask -H 'Content-Type: application/json' -d '{"q":"dalil sholat 5 waktu","limit":5}'
curl "localhost:20052/request?id=req_0123456789ab"
curl "localhost:20052/requests?limit=20"
```

- `POST /ask` (`{"q","limit"}`) dan `GET /ask?q=..&limit=..` → **202** `{"request_id","status":"queued","pertanyaan","limit"}`; `q` kosong → 400
- `GET /request?id=req_...` → `queued`/`running` = metadata saja; `done` = laporan lengkap (+`request_id`,`status`,`pertanyaan`,`durasi_detik`,`dibuat`); id tak dikenal → 404
- `GET /requests?limit=20` → riwayat ringkas terbaru dulu (maks 100)
- `/health` memuat `antrian` (queued+running), `workers`, `db`
- CORS: `Access-Control-Allow-Origin: *`, methods `GET, POST, OPTIONS`
- Pertanyaan identik yang masih antri < 15 menit mengembalikan `request_id` yang sama; hasil tersimpan di SQLite (`dari_cache` menandai jawaban dari cache pipeline)

## Antrian & persistensi

Semua permintaan lewat queue in-memory (`chan` + worker, `ST_WORKERS` default 1) dan dicatat di SQLite
(`requests`: id `req_`+12 hex, status `queued|running|done|error`, `hasil_json` = laporan penuh).
Hasil dari cache pipeline tetap dibuatkan row dengan `dari_cache=1`.

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

## Struktur

```
main.go        driver pipeline + cache + CLI
stages.go      6 tahap pipeline + prompt verbatim
llm.go         gateway LLM (trailer [DONE], fallback reasoning), ES/HTTP helpers
mcp.go         klien MCP stdio (Turath) + verifikasi halaman
store.go       SQLite request store + antrian/worker
server.go      /health /ask /request /requests /swagger/ + CORS, UI embedded
openapi.yaml   OpenAPI 3.0
static/        RapiDoc JS (embedded)
```
