package main

// main.go — driver pipeline, cache, dan CLI.

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const version = "1.0.0"

var defaultStages = []string{"keywords", "merge", "verify", "search", "curate"}

func logf(a ...any) {
	fmt.Fprintln(os.Stderr, append([]any{"[st]"}, a...)...)
}

func cacheKey(q string, stages []string) string {
	h := sha1.Sum([]byte(q + "|" + strings.Join(stages, ",")))
	return hex.EncodeToString(h[:])[:16]
}

func cacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "semantic-turath")
}

// run menjalankan pipeline; cache disimpan hanya untuk run penuh (sampai
// curate) dan kunci TIDAK memuat `limit`.
func run(q string, limit int, stages []string, useCache bool) map[string]any {
	if len(stages) == 0 {
		stages = defaultStages
	}
	cpath := filepath.Join(cacheDir(), cacheKey(q, stages)+".json")
	if useCache {
		if b, err := os.ReadFile(cpath); err == nil {
			var rep map[string]any
			if json.Unmarshal(b, &rep) == nil {
				rep["dari_cache"] = true
				if h, ok := rep["hasil"].([]any); ok {
					rep["hasil"] = h[:min(len(h), limit)]
				}
				logf("cache hit:", cpath)
				return rep
			}
			logf("cache rusak, diabaikan:", cpath)
		}
	}

	t0 := time.Now()
	rep := map[string]any{"pertanyaan": q, "stages": map[string]any{}}
	st := rep["stages"].(map[string]any)

	logf("1/5 generate keyword:", strings.Join(genModels, ", "))
	kws, errs := stageKeywords(q)
	rep["keywords_mentah"] = kws
	perModel := map[string]int{}
	for _, m := range genModels {
		n := 0
		for _, k := range kws {
			if k.Model == m {
				n++
			}
		}
		perModel[m] = n
	}
	st["keywords"] = map[string]any{"jumlah": len(kws), "per_model": perModel, "error": errs}
	logf("   keyword mentah:", len(kws), "| error:", mapOrDash(errs))
	if !contains(stages, "merge") {
		return rep
	}

	logf("2/5 merge via", mergeModel)
	merged := stageMerge(q, kws)
	rep["keywords_merged"] = merged
	st["merge"] = map[string]any{"jumlah": len(merged)}
	logf("   merged:", len(merged))
	if !contains(stages, "verify") {
		return rep
	}

	logf("3/5 verifikasi relevansi via mercury-decide")
	scored, kept := stageVerify(q, merged)
	rep["keywords_scored"] = scored
	st["verify"] = map[string]any{"dinilai": len(scored), "lolos": len(kept), "ambang": scoreMin}
	logf("   lolos:", len(kept), "/", len(scored))
	if !contains(stages, "search") {
		return rep
	}

	logf("4/5 pencarian ES + Turath + Ketabonline")
	hits := stageSearch(kept)
	perSumber := map[string]int{}
	for _, h := range hits {
		perSumber[h.Sumber]++
	}
	st["search"] = map[string]any{"nukilan_mentah": len(hits), "per_sumber": perSumber}
	logf("   nukilan mentah:", len(hits), perSumber)
	if !contains(stages, "curate") {
		rep["hasil"] = hits[:min(len(hits), limit)]
		return rep
	}

	logf("5/6 kurasi via", curateModel)
	resetSearchNotes()
	final, note := stageCurate(q, hits, max(limit, curateMin))
	st["curate"] = map[string]any{"catatan": note}
	// Verifikasi halaman nukilan Turath (hanya yang akan ditampilkan) + backfill.
	final, vp := stageVerifyPages(final, limit)
	st["verify_pages"] = vp
	logf("   verifikasi halaman:", vp["dicek"], "dicek,", vp["lolos"], "lolos,", vp["gagal"], "gagal")

	logf("6/6 terjemahan via", translateModel)
	final, tnote := stageTranslate(q, final)
	st["translate"] = map[string]any{"jumlah": len(final), "model": translateModel, "catatan": tnote}
	if len(final) == 0 {
		logf("   peringatan: tidak ada hasil setelah verifikasi")
	}
	rep["hasil"] = final
	if notes := drainSearchNotes(); len(notes) > 0 {
		if sm, ok := st["search"].(map[string]any); ok {
			sm["catatan"] = strings.Join(notes, " | ")
		}
	}
	rep["durasi_detik"] = round(time.Since(t0).Seconds(), 1)
	rep["pertanyaan"] = q

	if err := os.MkdirAll(cacheDir(), 0o755); err != nil {
		logf("gagal simpan cache:", err)
	} else if b, err := marshalJSON(rep, ""); err != nil {
		logf("gagal simpan cache:", err)
	} else if err := os.WriteFile(cpath, b, 0o644); err != nil {
		logf("gagal simpan cache:", err)
	}
	logf("   selesai:", note, "|", rep["durasi_detik"], "detik")
	rep["hasil"] = final[:min(len(final), limit)]
	return rep
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func mapOrDash(m map[string]any) any {
	if len(m) == 0 {
		return "-"
	}
	return m
}

// -------------------------------------------------------------------- CLI

// reorderArgs memindahkan argumen posisi ke belakang.
// Go `flag` berhenti parsing di argumen non-flag pertama, jadi
// `semantic-turath "pertanyaan" --limit 5` akan mengabaikan --limit.
// CLI Python menerima urutan bebas, jadi samakan perilakunya.
func reorderArgs(args []string) []string {
	// flag Go menerima -x maupun --x, jadi nama dinormalkan tanpa tanda hubung.
	valueFlags := map[string]bool{"limit": true, "out": true, "stages": true, "serve": true, "request": true, "requests": true}
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
		if valueFlags[name] && !strings.Contains(a, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, pos...)
}

// requestsCount = flag --requests dengan nilai opsional: dipakai tanpa angka
// berarti tampilkan 20 riwayat terbaru. Default 0 = fitur tidak aktif.
type requestsCount int

func requestsFlag(def int) *requestsCount {
	n := requestsCount(def)
	return &n
}

func (r *requestsCount) String() string { return strconv.Itoa(int(*r)) }

// IsBoolFlag membuat "--requests" tanpa nilai tetap valid (flag mengirim "true").
func (r *requestsCount) IsBoolFlag() bool { return true }

func (r *requestsCount) Set(s string) error {
	switch s {
	case "", "true":
		*r = 20 // dipakai tanpa angka -> tampilkan 20 terbaru
		return nil
	case "false":
		*r = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("jumlah riwayat harus angka: %q", s)
	}
	*r = requestsCount(n)
	return nil
}

func main() {
	loadDotEnv(".env") // kredensial dari file .env (tidak di-commit); env yang sudah ada menang
	limit := flag.Int("limit", 20, "jumlah hasil maksimal")
	out := flag.String("out", "", "tulis JSON ke file ini")
	stagesFlag := flag.String("stages", "keywords,merge,verify,search,curate,translate", "tahap yang dijalankan, dipisah koma")
	serve := flag.Int("serve", 0, "jalankan HTTP server di port ini")
	reqID := flag.String("request", "", "cetak laporan tersimpan (request_id) dari SQLite")
	reqList := requestsFlag(0)
	showVersion := flag.Bool("version", false, "cetak versi lalu keluar")
	printCfg := flag.Bool("print-config", false, "cetak konfigurasi efektif (env) lalu keluar")
	flag.Var(reqList, "requests", "cetak riwayat ringkas (opsional jumlah, default 20)")
	flag.CommandLine.Parse(reorderArgs(os.Args[1:])) //nolint:errcheck

	if *printCfg {
		// Konfigurasi efektif SETELAH .env dimuat. Nilai rahasia di-mask.
		// Sumber: (.env) bila diisi loadDotEnv, (env) bila dari proses, (default) bila bukan keduanya.
		src := func(k string) string {
			if dotEnvKeys[k] {
				return "(.env)"
			}
			if os.Getenv(k) != "" {
				return "(env)"
			}
			return "(default)"
		}
		mask := func(s string) string {
			if s == "" {
				return "(KOSONG)"
			}
			if len(s) <= 8 {
				return "***"
			}
			return s[:6] + "..." + s[len(s)-4:]
		}
		mcp := strings.Join(mcpCommand(), " ")
		port := os.Getenv("PORT")
		fmt.Printf("ST_GW         = %s %s\n", gwURL(), src("ST_GW"))
		fmt.Printf("ST_ES         = %s %s\n", esURL(), src("ST_ES"))
		fmt.Printf("ST_KEY        = %s %s\n", mask(key()), src("ST_KEY"))
		fmt.Printf("ST_DB         = %s %s\n", dbPath(), src("ST_DB"))
		fmt.Printf("ST_WORKERS    = %d %s\n", workerCount(), src("ST_WORKERS"))
		fmt.Printf("ST_TURATH_MCP = %s %s\n", mcp, src("ST_TURATH_MCP"))
		if port != "" {
			fmt.Printf("PORT          = %s %s\n", port, src("PORT"))
		} else {
			fmt.Println("PORT          = (tidak diset)")
		}
		return
	}
	if *showVersion {
		fmt.Println(version)
		return
	}

	// Port server: --serve menang; kalau tidak ada, pakai env PORT (untuk pm2)
	// asalkan tidak ada pertanyaan posisi (supaya CLI query tidak berubah jadi server).
	port := *serve
	if port == 0 && flag.NArg() == 0 {
		if p, err := strconv.Atoi(os.Getenv("PORT")); err == nil {
			port = p
		}
	}
	if port != 0 {
		st, err := openStore(true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gagal buka DB:", err)
			os.Exit(1)
		}
		serveHTTP(port, st)
		return
	}

	// --request <id>: cetak laporan tersimpan lalu keluar.
	if *reqID != "" {
		st, err := openStore(false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gagal buka DB:", err)
			os.Exit(1)
		}
		row, ok, err := st.get(*reqID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gagal baca DB:", err)
			os.Exit(1)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "request tidak dikenal: %s\n", *reqID)
			os.Exit(1)
		}
		writeReport(reportOf(row), *out)
		return
	}

	// --requests [n]: cetak riwayat ringkas lalu keluar.
	if *reqList > 0 {
		st, err := openStore(false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gagal buka DB:", err)
			os.Exit(1)
		}
		rows, err := st.list(int(*reqList))
		if err != nil {
			fmt.Fprintln(os.Stderr, "gagal baca DB:", err)
			os.Exit(1)
		}
		writeReport(map[string]any{"jumlah": len(rows), "requests": rows}, *out)
		return
	}

	q := ""
	if a := flag.Arg(0); a != "" {
		q = a
	}
	if q == "" {
		fmt.Fprintln(os.Stderr, "pertanyaan wajib (atau pakai --serve)")
		os.Exit(2)
	}

	var stages []string
	for _, s := range strings.Split(*stagesFlag, ",") {
		if s = strings.TrimSpace(s); s != "" {
			stages = append(stages, s)
		}
	}
	rep := run(q, *limit, stages, true)
	writeReport(rep, *out)
}

// writeReport mencetak JSON ke stdout atau menulis ke file bila out diset.
func writeReport(rep map[string]any, out string) {
	b, err := marshalJSON(rep, " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gagal marshal:", err)
		os.Exit(1)
	}
	if out != "" {
		if err := os.WriteFile(out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "gagal tulis:", err)
			os.Exit(1)
		}
		fmt.Printf("-> %s\n%s\n", out, trunc(string(b), 1200))
		return
	}
	fmt.Println(string(b))
}
