package main

import (
	"path/filepath"
	"testing"
)

// Request yang tertinggal berstatus queued/running (mis. server restart saat
// proses berjalan) harus didorong ulang ke antrian saat store dibuka.
func TestRecoverUnfinished(t *testing.T) {
	t.Setenv("ST_DB", filepath.Join(t.TempDir(), "requests.db"))
	s, err := openStore(false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()

	for _, id := range []string{"req_aaaaaaaaaaaa", "req_bbbbbbbbbbbb"} {
		if _, err := s.db.Exec(`INSERT INTO requests(id, pertanyaan, limit_hasil, status, dibuat)
			VALUES(?, 'q', 5, ?, ?)`, id, "running", nowRFC()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE requests SET status='running' WHERE id='req_aaaaaaaaaaaa'`); err != nil {
		t.Fatal(err)
	}

	// Buka ulang dengan worker: baris queued/running direset ke queued + didorong.
	s2, err := openStore(false)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.db.Close()

	// Jumlah yang terlihat di antrian DB harus tetap 2 sebelum recovery dijalankan
	// pada instance ber-worker (instance ini tanpa worker, jadi status belum berubah).
	n, err := s2.antrian()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("antrian sebelum recovery = %d, mau 2", n)
	}

	// Panggil recovery manual dan pastikan keduanya masuk channel.
	s2.recoverUnfinished()
	if got := len(s2.ch); got != 2 {
		t.Fatalf("channel setelah recovery = %d, mau 2", got)
	}
	var status string
	if err := s2.db.QueryRow(`SELECT status FROM requests WHERE id='req_aaaaaaaaaaaa'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("status setelah recovery = %q, mau queued", status)
	}
}

func TestSubmitDedupeAndID(t *testing.T) {
	t.Setenv("ST_DB", filepath.Join(t.TempDir(), "requests.db"))
	s, err := openStore(false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()

	id1, err := s.submit("pertanyaan uji", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(id1) != 4+12 || id1[:4] != "req_" {
		t.Fatalf("format id = %q", id1)
	}
	id2, err := s.submit("pertanyaan uji", 5)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Fatalf("dedupe gagal: %s != %s", id2, id1)
	}
	row, ok, err := s.get(id1)
	if err != nil || !ok {
		t.Fatalf("get = %v %v", ok, err)
	}
	if row.Status != "queued" || row.Pertanyaan != "pertanyaan uji" {
		t.Fatalf("row = %+v", row)
	}
	if _, ok, _ := s.get("req_000000000000"); ok {
		t.Fatal("id tak dikenal harus ok=false")
	}
}
