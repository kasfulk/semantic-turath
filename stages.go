package main

// stages.go — 5 tahap pipeline. Prompt disalin verbatim dari semantic_turath.py.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var genModels = []string{
	"kn/muse-spark-1-3-contributor:free",
	"kn/mimo-v2-6-flash:free",
	"kn/hy3:free",
	"kn/step-3-7-flash:free",
}

const (
	mergeModel     = "agnes/agnes-3.0-flash"
	curateModel    = "agnes/agnes-3.0-flash"
	translateModel = "kn-b/deepseek-v4-1-flash"
	decideModel    = "openrouter/inception/mercury-decide:free"

	scoreMin       = 0.5
	perSource      = 8
	candidateChars = 700
	candidateCap   = 60
	curateBatch    = 20
	curateMin      = 20
	translateBatch = 10
)

// ------------------------------------------------------------- 1. keyword gen

const genSystem = "Kamu asisten takhrij nukilan ulama. Jawab HANYA JSON valid, tanpa penjelasan, tanpa pagar kode, tanpa teks lain."

const genPromptTpl = `Pertanyaan user: "{q}"

Hasilkan keyword pencarian berbahasa Arab (frasa/kutipan ulama) yang relevan dengan pertanyaan itu,
dalam tiga kelompok:
- "kalimat_2": 4 keyword, setiap keyword minimal 2 kalimat Arab.
- "kalimat_3": 3 keyword, setiap keyword minimal 3 kalimat Arab.
- "kalimat_4": 2 keyword, setiap keyword minimal 4 kalimat Arab.

Aturan: pakai istilah fiqh yang dipakai ulama; tanpa terjemahan; tanpa nomor; tanpa penjelasan.
Format balasan persis:
{"kalimat_2":["...","...","...","..."],"kalimat_3":["...","...","..."],"kalimat_4":["...","..."]}`

// Keyword = satu frasa Arab beserta metadata asalnya.
type Keyword struct {
	Text       string `json:"text"`
	MinKalimat int    `json:"min_kalimat"`
	Model      string `json:"model"`
}

// ScoredKeyword = keyword setelah tahap verify.
type ScoredKeyword struct {
	Keyword
	Skor  *float64 `json:"skor"`
	Error string   `json:"error,omitempty"`
}

func genPrompt(q string) string {
	return strings.ReplaceAll(genPromptTpl, "{q}", q)
}

// normalizeKeywords menerima bentuk apa pun dari model -> []Keyword.
func normalizeKeywords(obj any, model string) []Keyword {
	out := []Keyword{}
	add := func(text string, min int) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if min == 0 {
			min = sentences(text)
		}
		out = append(out, Keyword{Text: text, MinKalimat: min, Model: model})
	}
	switch v := obj.(type) {
	case map[string]any:
		for k, val := range v {
			n := 0
			switch k {
			case "kalimat_2":
				n = 2
			case "kalimat_3":
				n = 3
			case "kalimat_4":
				n = 4
			}
			switch items := val.(type) {
			case []any:
				for _, t := range items {
					if s, ok := t.(string); ok {
						add(s, n)
					}
				}
			case string:
				add(items, sentences(items))
			}
		}
	case []any:
		for _, t := range v {
			switch item := t.(type) {
			case map[string]any:
				txt := ""
				for _, key := range []string{"keyword", "text", "arabic"} {
					if s := asString(item[key]); s != nil && *s != "" {
						txt = *s
						break
					}
				}
				if txt != "" {
					add(txt, sentences(txt))
				}
			case string:
				add(item, sentences(item))
			}
		}
	}
	return out
}

func genOne(model, q string) ([]Keyword, string) {
	raw, err := chat(model, genPrompt(q), genSystem, 6000, 600*time.Second)
	if err != nil {
		return nil, err.Error()
	}
	kws := normalizeKeywords(firstJSON(raw), model)
	if len(kws) == 0 {
		return nil, "RuntimeError: tidak ada keyword terparse"
	}
	return kws, ""
}

func stageKeywords(q string) ([]Keyword, map[string]any) {
	type res struct {
		kws []Keyword
		err string
	}
	results := make([]res, len(genModels))
	var wg sync.WaitGroup
	for i, m := range genModels {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			kws, err := genOne(m, q)
			results[i] = res{kws, err}
		}(i, m)
	}
	wg.Wait()

	keywords := []Keyword{}
	errors := map[string]any{}
	for i, r := range results {
		keywords = append(keywords, r.kws...)
		if r.err != "" {
			errors[genModels[i]] = r.err
		}
	}
	return keywords, errors
}

// ------------------------------------------------------------------ 2. merge

const mergePromptTpl = `Pertanyaan user: "{q}"

Berikut daftar keyword Arab dari beberapa model:
{items}

Gabungkan menjadi SATU array keyword unik yang relevan dengan pertanyaan user.
Buang duplikat/parafrase yang sama, pertahankan keyword yang paling lengkap kalimatnya.
Balas HANYA JSON array of string, urut dari yang paling relevan. Maksimal 30 item.`

func stageMerge(q string, keywords []Keyword) []Keyword {
	parts := make([]string, len(keywords))
	for i, k := range keywords {
		parts[i] = fmt.Sprintf("%d. [%d kalimat] %s", i+1, k.MinKalimat, k.Text)
	}
	prompt := strings.NewReplacer(
		"{q}", q,
		"{items}", strings.Join(parts, "\n"),
	).Replace(mergePromptTpl)

	merged := []Keyword{}
	raw, err := chat(mergeModel, prompt, genSystem, 8000, 600*time.Second)
	if err == nil {
		for _, t := range asList(firstJSON(raw)) {
			if s, ok := t.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					merged = append(merged, Keyword{Text: s, MinKalimat: sentences(s), Model: "merged"})
				}
			}
		}
	}
	if len(merged) == 0 { // fallback mekanis kalau agnes gagal
		sorted := make([]Keyword, len(keywords))
		copy(sorted, keywords)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].MinKalimat > sorted[j].MinKalimat })
		seen := map[string]bool{}
		for _, k := range sorted {
			key := spaceRe.ReplaceAllString(k.Text, " ")
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, Keyword{Text: k.Text, MinKalimat: k.MinKalimat, Model: "dedup-lokal"})
		}
	}
	if len(merged) > 30 {
		merged = merged[:30]
	}
	return merged
}

// ----------------------------------------------------------------- 3. verify

func decideScore(keyword, q string) (float64, error) {
	body := map[string]any{
		"model": decideModel,
		"state": keyword,
		"questions": map[string]any{
			"relevan": map[string]any{
				"type":         "noul",
				"instructions": fmt.Sprintf("apakah keyword ini relevan dengan pertanyaan: %s?", q),
			},
		},
	}
	raw, err := postJSON(GW+"/systemone", body,
		map[string]string{"Authorization": "Bearer " + key()}, 120*time.Second)
	if err != nil {
		return 0, err
	}
	var d struct {
		Answers map[string]struct {
			Noul float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(unwrap(raw)), &d); err != nil {
		return 0, err
	}
	return d.Answers["relevan"].Noul, nil
}

func stageVerify(q string, keywords []Keyword) ([]ScoredKeyword, []ScoredKeyword) {
	scored := make([]ScoredKeyword, len(keywords))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, k := range keywords {
		wg.Add(1)
		go func(i int, k Keyword) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out := ScoredKeyword{Keyword: k}
			if s, err := decideScore(k.Text, q); err == nil {
				v := round(s, 4)
				out.Skor = &v
			} else {
				out.Error = err.Error()
			}
			scored[i] = out
		}(i, k)
	}
	wg.Wait()

	kept := []ScoredKeyword{}
	for _, s := range scored {
		if s.Skor != nil && *s.Skor >= scoreMin {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 { // jangan matikan pipeline kalau semua di bawah ambang
		top := make([]ScoredKeyword, len(scored))
		copy(top, scored)
		sort.SliceStable(top, func(i, j int) bool {
			return skorOf(top[i]) > skorOf(top[j])
		})
		for i := range min(len(top), 5) {
			kept = append(kept, top[i])
		}
	}
	return scored, kept
}

func skorOf(s ScoredKeyword) float64 {
	if s.Skor == nil {
		return 0
	}
	return *s.Skor
}

// ----------------------------------------------------------------- 4. search

// Result = satu nukilan dari salah satu sumber.
type Result struct {
	Sumber      string   `json:"sumber"`
	JudulKitab  *string  `json:"judul_kitab"`
	Penulis     *string  `json:"penulis"`
	Kategori    *string  `json:"kategori,omitempty"`
	KitabID     any      `json:"kitab_id"`
	Lokasi      *string  `json:"lokasi"`
	Nukilan     string   `json:"nukilan"`
	Keyword     string   `json:"keyword"`
	SkorMekanis float64  `json:"skor_mekanis"`
	SkorDecide  *float64 `json:"skor_decide"`
	Terjemahan  string   `json:"terjemahan,omitempty"`
	Alasan      *string  `json:"alasan,omitempty"`

	// TurathMCP menandai nukilan Turath dari MCP (perlu verifikasi halaman).
	// Unexported-tidak; di-exclude dari JSON supaya kontrak keluaran tetap sama.
	TurathMCP bool `json:"-"`
}

var lokasiRe = regexp.MustCompile(`الجزء:\s*(\d+)\s*¦\s*الصفحة:\s*(\d+)`)

func srcES(kw string) ([]Result, error) {
	body := map[string]any{
		"size": perSource,
		"query": map[string]any{"bool": map[string]any{
			"should": []any{
				map[string]any{"match_phrase": map[string]any{"isi_teks": map[string]any{"query": kw, "boost": 3}}},
				map[string]any{"match": map[string]any{"isi_teks": map[string]any{"query": kw, "minimum_should_match": "60%", "boost": 1}}},
			},
			"minimum_should_match": 1,
		}},
		"highlight": map[string]any{"fields": map[string]any{
			"isi_teks": map[string]any{"fragment_size": 600, "number_of_fragments": 1},
		}},
	}
	raw, err := postJSON(ESURL, body, nil, 60*time.Second)
	if err != nil {
		return nil, err
	}
	var d struct {
		Hits struct {
			Hits []map[string]any `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, err
	}
	out := []Result{}
	for _, h := range d.Hits.Hits {
		s := asMap(h["_source"])
		hl := strings.Join(listStrings(asMap(h["highlight"])["isi_teks"]), " ")
		if hl == "" {
			if v, ok := s["isi_teks"].(string); ok {
				hl = v
			}
		}
		hl = stripHTML(hl)
		r := Result{
			Sumber:     "Maktabah Syamilah",
			JudulKitab: asString(s["judul"]),
			Penulis:    asString(s["penulis"]),
			Kategori:   asString(s["kategori"]),
			KitabID:    s["kitab_id"],
			Nukilan:    trunc(hl, 1200),
			Keyword:    kw,
		}
		if m := lokasiRe.FindStringSubmatch(hl); m != nil {
			loc := fmt.Sprintf("juz %s hlm %s", m[1], m[2])
			r.Lokasi = &loc
		}
		out = append(out, r)
	}
	return out, nil
}

// srcTurath mengambil nukilan Turath lewat MCP stdio (turath_search). Kalau MCP
// gagal, jatuh ke HTTP langsung (perilaku lama) dan mencacat fallback.
func srcTurath(kw string) ([]Result, error) {
	text, err := mcpToolText("turath_search", map[string]any{"query": kw, "page": 1}, mcpTimeout)
	if err != nil {
		addSearchNote("Turath via HTTP (fallback): " + err.Error())
		return srcTurathHTTP(kw)
	}
	var d struct {
		Count int `json:"count"`
		Data  []struct {
			BookID   any    `json:"book_id"`
			CatID    any    `json:"cat_id"`
			AuthorID any    `json:"author_id"`
			Meta     string `json:"meta"`
			Snip     string `json:"snip"`
			Text     string `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		addSearchNote("Turath balasan MCP tidak valid: " + err.Error())
		return srcTurathHTTP(kw)
	}
	items := d.Data
	if len(items) > perSource {
		items = items[:perSource]
	}
	out := []Result{}
	for _, m := range items {
		meta := map[string]any{}
		if m.Meta != "" {
			var parsed any
			if json.Unmarshal([]byte(m.Meta), &parsed) == nil {
				meta = asMap(parsed)
			}
		}
		snip := m.Snip
		if strings.TrimSpace(snip) == "" {
			snip = m.Text
		}
		loc := fmt.Sprintf("juz %s hlm %s", pyStr(meta["vol"]), pyStr(meta["page"]))
		out = append(out, Result{
			Sumber:     "Turath.io",
			JudulKitab: asString(meta["book_name"]),
			Penulis:    asString(meta["author_name"]),
			KitabID:    m.BookID,
			Lokasi:     &loc,
			Nukilan:    trunc(stripHTML(snip), 1200),
			Keyword:    kw,
			TurathMCP:  true,
		})
	}
	return out, nil
}

// srcTurathHTTP = perilaku lama (langsung ke api.turath.io).
func srcTurathHTTP(kw string) ([]Result, error) {
	d, err := jget(turathURL+"search", url.Values{"q": {kw}, "ver": {"3"}}, 60*time.Second)
	if err != nil {
		return nil, err
	}
	items := asList(asMap(d)["data"])
	if len(items) > perSource {
		items = items[:perSource]
	}
	out := []Result{}
	for _, it := range items {
		m := asMap(it)
		meta := map[string]any{}
		if s, ok := m["meta"].(string); ok && s != "" {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				meta = asMap(parsed)
			}
		}
		loc := fmt.Sprintf("juz %s hlm %s", pyStr(meta["vol"]), pyStr(meta["page"]))
		out = append(out, Result{
			Sumber:     "Turath.io",
			JudulKitab: asString(meta["book_name"]),
			Penulis:    asString(meta["author_name"]),
			KitabID:    m["book_id"],
			Lokasi:     &loc,
			Nukilan:    trunc(stripHTML(pyStr(m["snip"])), 1200),
			Keyword:    kw,
		})
	}
	return out, nil
}

func srcKetab(kw string) ([]Result, error) {
	d, err := jget(ketabURL, url.Values{"q": {kw}, "page": {"1"}}, 60*time.Second)
	if err != nil {
		return nil, err
	}
	items := asList(asMap(d)["data"])
	if len(items) > perSource {
		items = items[:perSource]
	}
	out := []Result{}
	for _, it := range items {
		m := asMap(it)
		book := asMap(m["book"])
		part := asMap(m["part"])
		content, _ := m["content"].(string)
		if content == "" {
			content, _ = m["html"].(string)
		}
		loc := fmt.Sprintf("juz %s hlm %s", pyStr(part["name"]), pyStr(m["page"]))
		out = append(out, Result{
			Sumber:     "Ketabonline",
			JudulKitab: asString(book["title"]),
			Penulis:    nil,
			KitabID:    book["id"],
			Lokasi:     &loc,
			Nukilan:    trunc(stripHTML(content), 1200),
			Keyword:    kw,
		})
	}
	return out, nil
}

// kwOverlap = rasio token keyword (>=3 huruf) yang muncul di nukilan.
func kwOverlap(kw, text string) float64 {
	toks := []string{}
	for _, t := range spaceRe.Split(kw, -1) {
		if len([]rune(t)) >= 3 {
			toks = append(toks, t)
		}
	}
	if len(toks) == 0 {
		return 0
	}
	n := 0
	for _, t := range toks {
		if strings.Contains(text, t) {
			n++
		}
	}
	return float64(n) / float64(len(toks))
}

func stageSearch(keywords []ScoredKeyword) []Result {
	type job struct {
		fn func(string) ([]Result, error)
		k  ScoredKeyword
	}
	jobs := []job{}
	for _, k := range keywords {
		for _, fn := range []func(string) ([]Result, error){srcES, srcTurath, srcKetab} {
			jobs = append(jobs, job{fn, k})
		}
	}
	batches := make([][]Result, len(jobs))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := j.fn(j.k.Text)
			if err == nil {
				for idx := range out {
					out[idx].SkorDecide = j.k.Skor
				}
			}
			batches[i] = out
		}(i, j)
	}
	wg.Wait()

	results := []Result{}
	seen := map[string]bool{}
	for _, batch := range batches {
		for _, r := range batch {
			if r.Nukilan == "" {
				continue
			}
			key := r.Sumber + "\x00" + trunc(spaceRe.ReplaceAllString(r.Nukilan, " "), 160)
			if seen[key] {
				continue
			}
			seen[key] = true
			r.SkorMekanis = round(kwOverlap(r.Keyword, r.Nukilan), 3)
			results = append(results, r)
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].SkorMekanis != results[j].SkorMekanis {
			return results[i].SkorMekanis > results[j].SkorMekanis
		}
		return results[i].Sumber < results[j].Sumber
	})
	return results
}

// ----------------------------------------------------------------- 5. curate

const curatePromptTpl = `Pertanyaan user: "{q}"

Berikut {n} nukilan dari kitab kuning (Maktabah Syamilah / Turath.io / Ketabonline):
{items}

Tugas:
1. Buang nukilan yang tidak menjawab pertanyaan (indeks, katalog, definisi kamus, gramatika, teks rusak).
2. Urutkan sisanya dari paling relevan.

Balas HANYA JSON array, maksimal {limit} item:
[{"idx":<nomor nukilan>,"relevan":true,"alasan":"<1 frasa>"}]`

// translatePromptTpl = prompt tahap terjemah (terpisah dari kurasi).
const translatePromptTpl = `Pertanyaan user: "{q}"

Berikut {n} nukilan kitab kuning yang sudah lolos kurasi:
{items}

Tugas: terjemahkan SETIAP nukilan di atas ke Bahasa Indonesia (ringkas, setia makna,
tanpa komentar, tanpa tashih, tanpa catatan tambahan).

Balas HANYA JSON array datar (bukan objek), berisi tepat {n} item, satu per nukilan,
urut nomor nukilan, tanpa pagar kode dan tanpa teks lain:
[{"idx":0,"terjemahan":"..."},{"idx":1,"terjemahan":"..."}]`

// truthy meniru Python: None/False/0/"" itu falsy.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	}
	return true
}

func stageCurate(q string, results []Result, limit int) ([]Result, string) {
	if len(results) == 0 {
		return []Result{}, "tidak ada nukilan"
	}
	cand := results
	if len(cand) > candidateCap {
		cand = cand[:candidateCap]
	}
	out := []Result{}
	dropped := 0
	notes := []string{}
	for start := 0; start < len(cand); start += curateBatch {
		if len(out) >= limit {
			break
		}
		end := start + curateBatch
		if end > len(cand) {
			end = len(cand)
		}
		chunk := cand[start:end]
		parts := make([]string, len(chunk))
		for i, r := range chunk {
			parts[i] = fmt.Sprintf("[%d] (%s) %s — %s\n%s",
				i, r.Sumber, orDash(r.JudulKitab), orDash(r.Penulis),
				trunc(r.Nukilan, candidateChars))
		}
		prompt := strings.NewReplacer(
			"{q}", q,
			"{n}", fmt.Sprint(len(chunk)),
			"{items}", strings.Join(parts, "\n\n"),
			"{limit}", fmt.Sprint(limit),
		).Replace(curatePromptTpl)

		raw, err := chat(curateModel, prompt, genSystem, 12000, 600*time.Second)
		if err != nil {
			notes = append(notes, fmt.Sprintf("batch %d: %s", start, err.Error()))
			continue
		}
		for _, v := range asList(firstJSON(raw)) {
			m, ok := v.(map[string]any)
			if !ok {
				dropped++
				continue
			}
			if rel, present := m["relevan"]; present && !truthy(rel) {
				dropped++
				continue
			}
			idx := asIdx(m["idx"], len(chunk))
			if idx < 0 {
				continue
			}
			r := chunk[idx]
			r.Alasan = asString(m["alasan"])
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}
	note := fmt.Sprintf("%d nukilan lolos dari %d kandidat, %d dibuang", len(out), len(cand), dropped)
	if len(notes) > 0 {
		note += " | error: " + strings.Join(notes, "; ")
	}
	return out, note
}

// stageTranslate menerjemahkan nukilan yang akan ditampilkan (hanya yang lolos
// kurasi) ke Bahasa Indonesia, batch translateBatch per panggilan. Batch yang
// gagal hanya dicatat, tidak menghentikan pipeline.
func stageTranslate(q string, results []Result) ([]Result, string) {
	if len(results) == 0 {
		return results, "tidak ada nukilan untuk diterjemahkan"
	}
	notes := []string{}
	for start := 0; start < len(results); start += translateBatch {
		end := min(start+translateBatch, len(results))
		chunk := results[start:end] // slice berbagi backing array: mutasi di sini ikut terlihat
		parts := make([]string, len(chunk))
		for i, r := range chunk {
			parts[i] = fmt.Sprintf("[%d] (%s) %s — %s\n%s",
				i, r.Sumber, orDash(r.JudulKitab), orDash(r.Penulis),
				trunc(r.Nukilan, candidateChars))
		}
		prompt := strings.NewReplacer(
			"{q}", q,
			"{n}", fmt.Sprint(len(chunk)),
			"{items}", strings.Join(parts, "\n\n"),
		).Replace(translatePromptTpl)

		raw, err := chat(translateModel, prompt, genSystem, 8000, 600*time.Second)
		if err != nil {
			notes = append(notes, fmt.Sprintf("batch %d: %s", start, err.Error()))
			continue
		}
		n := 0
		for _, m := range extractTranslated(firstJSON(raw)) {
			idx, ok := itemIdx(m, len(chunk))
			if !ok {
				continue
			}
			if s := itemText(m); s != "" {
				chunk[idx].Terjemahan = s
				n++
			}
		}
		if n == 0 {
			notes = append(notes, fmt.Sprintf("batch %d: tidak ada terjemahan terparse", start))
		}
	}
	n := 0
	for _, r := range results {
		if r.Terjemahan != "" {
			n++
		}
	}
	note := fmt.Sprintf("%d/%d nukilan diterjemahkan", n, len(results))
	if len(notes) > 0 {
		note += " | error: " + strings.Join(notes, "; ")
	}
	return results, note
}

// extractTranslated menormalkan bentuk balasan model jadi daftar item: array
// langsung, {"results":[...]}, {"data":[...]}, atau satu objek tunggal.
func extractTranslated(v any) []map[string]any {
	out := []map[string]any{}
	add := func(x any) {
		switch t := x.(type) {
		case map[string]any:
			out = append(out, t)
		case []any:
			for _, it := range t {
				if m, ok := it.(map[string]any); ok {
					out = append(out, m)
				}
			}
		}
	}
	switch t := v.(type) {
	case []any:
		add(t)
	case map[string]any:
		found := false
		for _, k := range []string{"results", "data", "items", "hasil", "translations"} {
			switch inner := t[k].(type) {
			case []any, map[string]any:
				add(inner)
				found = true
			}
		}
		if !found {
			add(t)
		}
	}
	return out
}

// itemIdx menerima penanda nomor nukilan dari model (idx/id/nomor/number/index),
// bisa angka JSON atau string.
func itemIdx(m map[string]any, n int) (int, bool) {
	for _, k := range []string{"idx", "id", "nomor", "number", "index", "no"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		if i := asIdx(v, n); i >= 0 {
			return i, true
		}
		if i, ok := asInt(v); ok && i >= 0 && i < n {
			return i, true
		}
	}
	return 0, false
}

// itemText menerima teks terjemahan dari beberapa nama field yang mungkin.
func itemText(m map[string]any) string {
	for _, k := range []string{"terjemahan", "translation", "arti", "text", "teks", "quote", "hasil"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func orDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}
