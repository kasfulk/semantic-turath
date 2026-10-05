package main

// server.go — HTTP service: /health, /ask, /swagger/ (+ CORS).

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed openapi.yaml
var openapiYAML []byte

//go:embed static/rapidoc-min.js
var rapidocJS []byte

const swaggerHTML = `<!doctype html>
<html lang="id">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Semantic Turath — API</title>
<script type="module" src="/swagger/rapidoc-min.js"></script>
</head>
<body style="margin:0">
<rapi-doc spec-url="/swagger/openapi.yaml" render-style="tree" theme="dark"
  show-header="false" allow-try="true" router="hash"></rapi-doc>
</body>
</html>
`

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := marshalJSON(v, "")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func serveHTTP(port int, st *store) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		antrian, err := st.antrian()
		db := "ok"
		if err == nil && st.ping() != nil {
			db = "error"
		}
		if err != nil {
			db = "error"
		}
		writeJSON(w, 200, map[string]any{
			"status":  "ok",
			"sumber":  []string{"konten_kitab", "turath.io", "ketabonline"},
			"port":    port,
			"antrian": antrian,
			"workers": st.workers,
			"db":      db,
		})
	})
	// askHandler menerima POST {"q","limit"} atau GET /ask?q=..&limit=.. lalu
	// mengembalikan 202 dengan request_id (async, tidak blokir).
	askHandler := func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("RuntimeError: %v", rec)})
			}
		}()
		q, limit := "", 20
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			if r.Body != nil {
				_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
			}
			q = strings.TrimSpace(body.Q)
			if body.Limit > 0 {
				limit = body.Limit
			}
		case http.MethodGet:
			q = strings.TrimSpace(r.URL.Query().Get("q"))
			if s := r.URL.Query().Get("limit"); s != "" {
				if n, err := strconv.Atoi(s); err == nil {
					limit = n
				}
			}
		default:
			writeJSON(w, 404, map[string]string{"error": "pakai POST /ask {\"q\":\"...\",\"limit\":10} atau GET /ask?q=<pertanyaan>&limit=20"})
			return
		}
		if q == "" {
			writeJSON(w, 400, map[string]string{"error": "parameter q wajib"})
			return
		}
		limit = min(max(limit, 1), 50)
		id, err := st.submit(q, limit)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "gagal simpan request: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": id,
			"status":     "queued",
			"pertanyaan": q,
			"limit":      limit,
		})
	}
	mux.HandleFunc("/ask", askHandler)
	mux.HandleFunc("/request", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, 400, map[string]string{"error": "parameter id wajib"})
			return
		}
		row, ok, err := st.get(id)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "request tidak dikenal: " + id})
			return
		}
		writeJSON(w, 200, reportOf(row))
	})
	mux.HandleFunc("/requests", func(w http.ResponseWriter, r *http.Request) {
		limit := 20
		if s := r.URL.Query().Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				limit = n
			}
		}
		rows, err := st.list(limit)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"jumlah": len(rows), "requests": rows})
	})
	mux.HandleFunc("/swagger/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/swagger/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(swaggerHTML))
		case "/swagger/openapi.yaml":
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
			_, _ = w.Write(openapiYAML)
		case "/swagger/rapidoc-min.js":
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			_, _ = w.Write(rapidocJS)
		default:
			writeJSON(w, 404, map[string]string{"error": "pakai /swagger/"})
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/swagger" {
			http.Redirect(w, r, "/swagger/", http.StatusFound)
			return
		}
		writeJSON(w, 404, map[string]string{"error": "pakai POST /ask {\"q\":\"...\"} atau GET /ask?q=<pertanyaan>&limit=20"})
	})

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: withCORS(mux), ReadHeaderTimeout: 10 * time.Second}
	logf(fmt.Sprintf("serve http://0.0.0.0:%d/ask?q=...", port))
	_ = srv.ListenAndServe()
}
