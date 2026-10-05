package main

// server.go — HTTP service: /health, /ask, /swagger/ (+ CORS).

import (
	_ "embed"
	"fmt"
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
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func serveHTTP(port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"status": "ok",
			"sumber": []string{"konten_kitab", "turath.io", "ketabonline"},
			"port":   port,
		})
	})
	mux.HandleFunc("/ask", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("RuntimeError: %v", rec)})
			}
		}()
		if r.Method != http.MethodGet {
			writeJSON(w, 404, map[string]string{"error": "pakai /ask?q=<pertanyaan>&limit=20"})
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			writeJSON(w, 400, map[string]string{"error": "parameter q wajib"})
			return
		}
		limit := 20
		if s := r.URL.Query().Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				limit = n
			}
		}
		limit = min(max(limit, 1), 50)
		writeJSON(w, 200, run(q, limit, defaultStages, true))
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
		writeJSON(w, 404, map[string]string{"error": "pakai /ask?q=<pertanyaan>&limit=20"})
	})

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: withCORS(mux), ReadHeaderTimeout: 10 * time.Second}
	logf(fmt.Sprintf("serve http://0.0.0.0:%d/ask?q=...", port))
	_ = srv.ListenAndServe()
}
