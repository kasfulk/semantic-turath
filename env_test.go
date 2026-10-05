package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Regresi: dulu GW/ESURL dibaca saat package init (sebelum .env dimuat), jadi
// nilai dari .env terkunci ke default. Semua variabel harus dibaca lazy
// (saat dipakai), bukan saat init.
func TestEnvFromDotEnvBeatsDefaults(t *testing.T) {
	dir := t.TempDir()
	content := "ST_GW=http://contoh-invalid:9999/v1\n" +
		"ST_ES=http://contoh-invalid:9999/es\n" +
		"ST_DB=" + filepath.Join(dir, "uji.db") + "\n" +
		"ST_WORKERS=3\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	keys := []string{"ST_GW", "ST_ES", "ST_DB", "ST_WORKERS"}
	for _, k := range keys {
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, k := range keys {
			_ = os.Unsetenv(k)
			delete(dotEnvKeys, k)
		}
	})

	loadDotEnv(filepath.Join(dir, ".env"))

	if got := gwURL(); got != "http://contoh-invalid:9999/v1" {
		t.Errorf("gwURL() = %q, mau nilai dari .env http://contoh-invalid:9999/v1", got)
	}
	if got := esURL(); got != "http://contoh-invalid:9999/es" {
		t.Errorf("esURL() = %q, mau nilai dari .env http://contoh-invalid:9999/es", got)
	}
	if got := dbPath(); got != filepath.Join(dir, "uji.db") {
		t.Errorf("dbPath() = %q, mau nilai dari .env %s", got, filepath.Join(dir, "uji.db"))
	}
	if got := workerCount(); got != 3 {
		t.Errorf("workerCount() = %d, mau 3", got)
	}
	if !dotEnvKeys["ST_GW"] {
		t.Error("dotEnvKeys tidak menandai ST_GW berasal dari .env (dipakai --print-config)")
	}
}
