package main

// store.go — antrian request + persistensi SQLite (modernc.org/sqlite, tanpa cgo).
// Semua permintaan lewat queue; user cukup menyimpan request_id.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS requests(
  id           TEXT PRIMARY KEY,
  pertanyaan   TEXT NOT NULL,
  limit_hasil  INTEGER NOT NULL,
  status       TEXT NOT NULL,
  dibuat       TEXT NOT NULL,
  mulai        TEXT,
  selesai      TEXT,
  durasi_detik REAL,
  error        TEXT,
  hasil_json   TEXT,
  dari_cache   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_requests_dibuat ON requests(dibuat DESC);
`

// store = DB + antrian in-memory (chan id + worker).
type store struct {
	db      *sql.DB
	ch      chan string
	workers int
}

// requestFull = satu baris requests (dipakai /request dan CLI --request).
type requestFull struct {
	ID         string
	Pertanyaan string
	Status     string
	Dibuat     string
	Mulai      *string
	Selesai    *string
	Durasi     *float64
	Err        *string
	HasilJSON  *string
	DariCache  int
}

// requestInfo = ringkasan satu baris untuk /requests dan CLI --requests.
type requestInfo struct {
	ID          string   `json:"request_id"`
	Pertanyaan  string   `json:"pertanyaan"`
	Status      string   `json:"status"`
	Dibuat      string   `json:"dibuat"`
	DurasiDetik *float64 `json:"durasi_detik"`
}

func dbPath() string {
	if p := strings.TrimSpace(os.Getenv("ST_DB")); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "semantic-turath", "requests.db")
}

func workerCount() int {
	if s := strings.TrimSpace(os.Getenv("ST_WORKERS")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 1 // pipeline berat, default satu worker
}

func nowRFC() string { return time.Now().Format(time.RFC3339) }

// openStore membuka DB (membuat direktori + skema bila belum ada). Worker hanya
// dinyalakan di mode server supaya CLI tidak ikut mengonsumsi antrian.
func openStore(withWorkers bool) (*store, error) {
	p := dbPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", p+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &store{db: db, ch: make(chan string, 256), workers: workerCount()}
	if withWorkers {
		s.recoverUnfinished()
		for range s.workers {
			go s.worker()
		}
	}
	return s, nil
}

// recoverUnfinished mendorong ulang request queued/running yang tertinggal
// (mis. server restart saat proses berjalan) supaya tidak nyangkut permanen.
func (s *store) recoverUnfinished() {
	rows, err := s.db.Query(`SELECT id FROM requests WHERE status IN ('queued','running') ORDER BY dibuat`)
	if err != nil {
		logf("recovery:", err)
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		if _, err := s.db.Exec(`UPDATE requests SET status='queued', mulai=NULL WHERE id=?`, id); err != nil {
			logf("recovery:", id, err)
			continue
		}
		s.ch <- id
	}
	if len(ids) > 0 {
		logf("recovery: mendorong ulang", len(ids), "request tertinggal")
	}
}

func newRequestID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("req_%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return "req_" + hex.EncodeToString(b)
}

// submit menyimpan request baru (queued), mendorong id ke antrian, dan langsung
// mengembalikan id. Pertanyaan identik yang masih queued/running < 15 menit
// dikembalikan apa adanya (dedupe ringan).
func (s *store) submit(q string, limit int) (string, error) {
	cutoff := time.Now().Add(-15 * time.Minute).Format(time.RFC3339)
	var id string
	err := s.db.QueryRow(`SELECT id FROM requests
		WHERE pertanyaan = ? AND status IN ('queued','running') AND dibuat >= ?
		ORDER BY dibuat DESC LIMIT 1`, q, cutoff).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	id = newRequestID()
	if _, err := s.db.Exec(`INSERT INTO requests(id, pertanyaan, limit_hasil, status, dibuat)
		VALUES(?, ?, ?, 'queued', ?)`, id, q, limit, nowRFC()); err != nil {
		return "", err
	}
	s.ch <- id
	return id, nil
}

func (s *store) worker() {
	for id := range s.ch {
		s.process(id)
	}
}

// process menjalankan satu request: running -> run() -> done/error (+ recover).
func (s *store) process(id string) {
	var q string
	var limit int
	if err := s.db.QueryRow(`SELECT pertanyaan, limit_hasil FROM requests WHERE id = ?`, id).
		Scan(&q, &limit); err != nil {
		logf("antrian:", id, err)
		return
	}
	t0 := time.Now()
	if _, err := s.db.Exec(`UPDATE requests SET status='running', mulai=? WHERE id=?`, nowRFC(), id); err != nil {
		logf("antrian:", id, err)
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			msg := fmt.Sprintf("panic: %v", rec)
			logf("antrian:", id, msg)
			_, _ = s.db.Exec(`UPDATE requests SET status='error', selesai=?, durasi_detik=?, error=? WHERE id=?`,
				nowRFC(), round(time.Since(t0).Seconds(), 1), msg, id)
		}
	}()

	rep := run(q, limit, defaultStages, true)
	b, err := marshalJSON(rep, "")
	if err != nil {
		_, _ = s.db.Exec(`UPDATE requests SET status='error', selesai=?, durasi_detik=?, error=? WHERE id=?`,
			nowRFC(), round(time.Since(t0).Seconds(), 1), "marshal: "+err.Error(), id)
		return
	}
	cached := 0
	if v, ok := rep["dari_cache"].(bool); ok && v {
		cached = 1
	}
	if _, err := s.db.Exec(`UPDATE requests
		SET status='done', selesai=?, durasi_detik=?, hasil_json=?, dari_cache=? WHERE id=?`,
		nowRFC(), round(time.Since(t0).Seconds(), 1), string(b), cached, id); err != nil {
		logf("antrian:", id, err)
	}
}

// get mengambil satu baris requests; ok=false kalau id tidak dikenal.
func (s *store) get(id string) (*requestFull, bool, error) {
	var r requestFull
	var mulai, selesai, errStr, hasil sql.NullString
	var durasi sql.NullFloat64
	err := s.db.QueryRow(`SELECT id, pertanyaan, status, dibuat, mulai, selesai, durasi_detik, error, hasil_json, dari_cache
		FROM requests WHERE id = ?`, id).
		Scan(&r.ID, &r.Pertanyaan, &r.Status, &r.Dibuat, &mulai, &selesai, &durasi, &errStr, &hasil, &r.DariCache)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if mulai.Valid {
		r.Mulai = &mulai.String
	}
	if selesai.Valid {
		r.Selesai = &selesai.String
	}
	if durasi.Valid {
		r.Durasi = &durasi.Float64
	}
	if errStr.Valid {
		r.Err = &errStr.String
	}
	if hasil.Valid {
		r.HasilJSON = &hasil.String
	}
	return &r, true, nil
}

// list mengambil riwayat terbaru dulu (maks 100).
func (s *store) list(limit int) ([]requestInfo, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, pertanyaan, status, dibuat, durasi_detik
		FROM requests ORDER BY dibuat DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []requestInfo{}
	for rows.Next() {
		var r requestInfo
		var durasi sql.NullFloat64
		if err := rows.Scan(&r.ID, &r.Pertanyaan, &r.Status, &r.Dibuat, &durasi); err != nil {
			return nil, err
		}
		if durasi.Valid {
			r.DurasiDetik = &durasi.Float64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// antrian = jumlah request queued + running.
func (s *store) antrian() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE status IN ('queued','running')`).Scan(&n)
	return n, err
}

func (s *store) ping() error { return s.db.Ping() }

// reportOf menyusun laporan HTTP/CLI dari satu baris requests: queued/running
// hanya status; done/error memuat laporan penuh (isi hasil_json + metadata).
func reportOf(r *requestFull) map[string]any {
	out := map[string]any{
		"request_id": r.ID,
		"status":     r.Status,
		"pertanyaan": r.Pertanyaan,
		"dibuat":     r.Dibuat,
	}
	if r.Mulai != nil {
		out["mulai"] = *r.Mulai
	}
	if r.Selesai != nil {
		out["selesai"] = *r.Selesai
	}
	if r.Durasi != nil {
		out["durasi_detik"] = *r.Durasi
	}
	if r.Err != nil && *r.Err != "" {
		out["error"] = *r.Err
	}
	if r.HasilJSON != nil && *r.HasilJSON != "" {
		var full map[string]any
		if json.Unmarshal([]byte(*r.HasilJSON), &full) == nil {
			for k, v := range out {
				full[k] = v
			}
			return full
		}
	}
	return out
}
