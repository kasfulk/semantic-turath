package main

// mcp.go — klien MCP stdio minimal untuk Turath + verifikasi halaman nukilan.
// Turath (satu-satunya pemakai MCP) lewat satu proses tunggal; ES dan Ketabonline
// tetap HTTP langsung.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"
)

const (
	mcpTimeout   = 60 * time.Second
	mcpProtocol  = "2024-11-05"
	verifyMax    = 12 // maksimum nukilan Turath yang dicek per permintaan
	verifySample = 8  // kata per potongan pembanding
)

type mcpRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *mcpError       `json:"error"`
}

// mcpClient membungkus proses MCP tunggal (stdio satu stream): id request naik,
// dispatch balasan per id, restart otomatis kalau proses mati.
type mcpClient struct {
	mu        sync.Mutex // serialisasi seluruh call (tulis + tunggu balasan)
	pendingMu sync.Mutex
	pending   map[int]chan mcpResponse
	cmd       *exec.Cmd
	in        io.WriteCloser
	done      chan struct{} // ditutup saat stdout proses habis (proses mati)
	nextID    int
}

var mcp = &mcpClient{pending: map[int]chan mcpResponse{}}

// mcpCommand membaca perintah server MCP dari ST_TURATH_MCP (kalau tidak diset,
// pakai perintah default).
func mcpCommand() []string {
	if s := strings.TrimSpace(os.Getenv("ST_TURATH_MCP")); s != "" {
		return strings.Fields(s)
	}
	return []string{"npx", "--yes", "tsx", "/home/kasjful-kurniawan/turath-mcp/src/index.ts"}
}

func (c *mcpClient) start() error {
	args := mcpCommand()
	cmd := exec.Command(args[0], args[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	c.cmd = cmd
	c.in = stdin
	c.done = done
	go c.readLoop(stdout, done)
	return nil
}

// readLoop membaca stdout baris-per-baris (NDJSON) dan mengirim balasan ke
// channel pemilik id-nya. Keluar saat stream habis (proses mati).
func (c *mcpClient) readLoop(r io.Reader, done chan struct{}) {
	defer close(done)
	dec := json.NewDecoder(r)
	for {
		var resp mcpResponse
		if err := dec.Decode(&resp); err != nil {
			break
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.pendingMu.Unlock()
		if ok {
			ch <- resp
		}
	}
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- mcpResponse{Error: &mcpError{Message: "mcp: proses mati"}}
	}
	c.pendingMu.Unlock()
}

// kill menutup proses dan menggagalkan semua request yang menunggu.
func (c *mcpClient) kill() {
	if c.in != nil {
		_ = c.in.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
	c.cmd, c.in, c.done = nil, nil, nil
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- mcpResponse{Error: &mcpError{Message: "mcp: proses dihentikan"}}
	}
	c.pendingMu.Unlock()
}

// ensure memastikan proses hidup dan handshake sudah selesai (lazy spawn).
func (c *mcpClient) ensure() error {
	if c.cmd != nil {
		select {
		case <-c.done:
			c.kill() // proses mati -> restart
		default:
			return nil
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.start(); err != nil {
			return fmt.Errorf("mcp: start gagal: %w", err)
		}
		if _, err := c.roundTrip("initialize", map[string]any{
			"protocolVersion": mcpProtocol,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "semantic-turath", "version": version},
		}, mcpTimeout); err != nil {
			c.kill()
			continue
		}
		if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
			c.kill()
			continue
		}
		return nil
	}
	return fmt.Errorf("mcp: handshake gagal")
}

func (c *mcpClient) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(b, '\n'))
	return err
}

// notify mengirim notifikasi (tanpa id, tanpa balasan).
func (c *mcpClient) notify(method string, params any) error {
	return c.write(mcpRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// roundTrip mengirim request ber-id dan menunggu balasannya (dipanggil dengan
// c.mu dipegang).
func (c *mcpClient) roundTrip(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	ch := make(chan mcpResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.write(mcpRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp %s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(timeout):
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("mcp %s: timeout %s", method, timeout)
	}
}

// call = ensure + roundTrip + restart otomatis satu kali kalau gagal.
func (c *mcpClient) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = mcpTimeout
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.ensure(); err != nil {
			lastErr = err
			continue
		}
		res, err := c.roundTrip(method, params, timeout)
		if err == nil {
			return res, nil
		}
		lastErr = err
		c.kill()
	}
	return nil, lastErr
}

// mcpToolText memanggil satu tool MCP dan mengembalikan content[0].text.
func mcpToolText(tool string, args map[string]any, timeout time.Duration) (string, error) {
	raw, err := mcp.call("tools/call", map[string]any{"name": tool, "arguments": args}, timeout)
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("mcp %s: balasan tidak valid: %w", tool, err)
	}
	for _, c := range res.Content {
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		if res.IsError {
			return "", fmt.Errorf("mcp %s: %s", tool, trunc(c.Text, 300))
		}
		return c.Text, nil
	}
	if res.IsError {
		return "", fmt.Errorf("mcp %s: error tanpa pesan", tool)
	}
	return "", fmt.Errorf("mcp %s: content kosong", tool)
}

// ------------------------------------------------- catatan pencarian (fallback)

var (
	searchNoteMu   sync.Mutex
	searchNotes    []string
	turathFallback bool
)

// addSearchNote mencatat catatan sumber Turath dan menandai mode fallback HTTP.
func addSearchNote(note string) {
	searchNoteMu.Lock()
	defer searchNoteMu.Unlock()
	searchNotes = append(searchNotes, note)
	turathFallback = true
}

func fallbackUsed() bool {
	searchNoteMu.Lock()
	defer searchNoteMu.Unlock()
	return turathFallback
}

// resetSearchNotes dipanggil di awal setiap run.
func resetSearchNotes() {
	searchNoteMu.Lock()
	defer searchNoteMu.Unlock()
	searchNotes = nil
	turathFallback = false
}

// drainSearchNotes mengambil catatan unik lalu mengosongkannya.
func drainSearchNotes() []string {
	searchNoteMu.Lock()
	defer searchNoteMu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, n := range searchNotes {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	searchNotes = nil
	return out
}

// ------------------------------------------------ verifikasi halaman (Turath)

var (
	// tatwil + harakat/tanda baca Arab
	harakatRe = regexp.MustCompile(`[\x{0640}\x{064B}-\x{065F}\x{0670}\x{06D6}-\x{06ED}\x{08F0}-\x{08F3}\x{FEFF}]`)
	pageRe    = regexp.MustCompile(`hlm\s+([0-9]+)`)
)

// normArabic merapikan teks untuk pencocokan: buang tag HTML, kompatibilitas
// normalisasi, tatwil, harakat, lalu rapikan spasi.
func normArabic(s string) string {
	s = stripHTML(s)
	s = norm.NFKC.String(s)
	s = harakatRe.ReplaceAllString(s, "")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// samplesNukilan mengambil potongan ~verifySample kata dari posisi berbeda
// (awal/tengah/akhir); kalau nukilan lebih pendek, pakai seluruhnya.
func samplesNukilan(nukilan string) []string {
	words := strings.Fields(normArabic(nukilan))
	if len(words) == 0 {
		return nil
	}
	if len(words) <= verifySample {
		return []string{strings.Join(words, " ")}
	}
	out := []string{}
	for _, start := range []int{0, len(words) / 3, 2 * len(words) / 3} {
		if start+verifySample > len(words) {
			start = len(words) - verifySample
		}
		out = append(out, strings.Join(words[start:start+verifySample], " "))
	}
	return out
}

// extractPageText menerima teks mentah tool turath_get_page; kalau balasannya
// JSON, ambil field teksnya.
func extractPageText(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		switch t := v.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				return t
			}
		case map[string]any:
			for _, k := range []string{"text", "content", "page_text", "body"} {
				if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	}
	return raw
}

// verifyTurathPage memastikan nukilan benar ada di halaman kitab sumbernya.
func verifyTurathPage(r Result) (bool, error) {
	bookID, ok := asInt(r.KitabID)
	if !ok {
		return false, fmt.Errorf("book_id tidak dikenal: %v", r.KitabID)
	}
	page := 0
	if r.Lokasi != nil {
		if m := pageRe.FindStringSubmatch(*r.Lokasi); m != nil {
			page, _ = strconv.Atoi(m[1])
		}
	}
	if page <= 0 {
		return false, fmt.Errorf("halaman tidak dikenal: %s", orDash(r.Lokasi))
	}
	raw, err := mcpToolText("turath_get_page", map[string]any{"book_id": bookID, "page": page}, mcpTimeout)
	if err != nil {
		return false, err
	}
	pageText := normArabic(extractPageText(raw))
	samples := samplesNukilan(r.Nukilan)
	if len(samples) == 0 {
		return false, fmt.Errorf("nukilan kosong")
	}
	for _, s := range samples {
		if s != "" && strings.Contains(pageText, s) {
			return true, nil
		}
	}
	return false, nil
}

// stageVerifyPages memverifikasi nukilan Turath yang akan ditampilkan (sekuensial,
// maks verifyMax pemeriksaan), membuang yang gagal, lalu backfill dari kandidat
// berikutnya (satu putaran). Hasil non-Turath dilewatkan tanpa verifikasi.
func stageVerifyPages(pool []Result, limit int) ([]Result, map[string]any) {
	info := map[string]any{"dicek": 0, "lolos": 0, "gagal": 0, "catatan": "-"}
	if len(pool) == 0 {
		info["catatan"] = "tidak ada kandidat"
		return []Result{}, info
	}
	if fallbackUsed() {
		info["catatan"] = "Turath memakai fallback HTTP, verifikasi halaman dilewati"
		return pool[:min(len(pool), limit)], info
	}

	// Jendela tampil = limit pertama; verifikasi juga kandidat Turath berikutnya
	// (backfill) selama anggaran verifyMax belum habis.
	window := min(len(pool), limit)
	head, rest := pool[:window], pool[window:]

	out := []Result{}
	checked, passed, failed := 0, 0, 0
	notes := []string{}
	round := func(cands []Result) {
		for _, r := range cands {
			if len(out) >= limit || checked >= verifyMax {
				return
			}
			if !r.TurathMCP {
				out = append(out, r)
				continue
			}
			checked++
			ok, err := verifyTurathPage(r)
			switch {
			case err != nil:
				failed++
				notes = append(notes, fmt.Sprintf("%s: %v", orDash(r.JudulKitab), err))
			case ok:
				passed++
				out = append(out, r)
			default:
				failed++
			}
		}
	}

	round(head)
	// Backfill: hanya kandidat Turath (nukilan non-Turath di luar jendela tidak
	// perlu diverifikasi dan tidak mengisi kekurangan).
	backfill := make([]Result, 0, len(rest))
	for _, r := range rest {
		if r.TurathMCP {
			backfill = append(backfill, r)
		}
	}
	round(backfill)

	info["dicek"], info["lolos"], info["gagal"] = checked, passed, failed
	switch {
	case checked == 0:
		info["catatan"] = "tidak ada nukilan Turath pada jendela tampil/backfill"
	case len(notes) > 0:
		info["catatan"] = fmt.Sprintf("%d lolos, %d gagal (%s)", passed, failed, strings.Join(notes, "; "))
	default:
		info["catatan"] = fmt.Sprintf("%d nukilan Turath lolos verifikasi halaman", passed)
	}
	if len(out) < limit && checked >= verifyMax {
		info["catatan"] = fmt.Sprintf("%s | batas %d pemeriksaan tercapai", info["catatan"], verifyMax)
	}
	return out, info
}

// asInt menerima angka JSON (float64), string, atau json.Number.
func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) {
			return int(t), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n, true
		}
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}
