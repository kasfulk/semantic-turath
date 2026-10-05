package main

// llm.go — akses gateway LLM + helper HTTP/JSON.
// Pitfall gateway (GO_SPEC bag.4): tanpa flag `stream`; trailer `data: [DONE]`
// menempel pada respons non-streaming; jawaban bisa di `reasoning` bukan `content`.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// loadDotEnv memuat pasangan KEY=VALUE dari .env bila variabelnya belum di-set.
// Nilai yang sudah ada di environment tidak ditimpa (env > .env > default).
func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" && os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

var (
	GW    = env("ST_GW", "http://localhost:20128/v1")
	ESURL = env("ST_ES", "http://localhost:9200/konten_kitab/_search")

	turathURL = "https://api.turath.io/"
	ketabURL  = "https://backend.ketabonline.com/api/v2/books/pages"
)

// key membaca kredensial gateway saat dipanggil (setelah .env dimuat).
func key() string { return os.Getenv("ST_KEY") }

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func postJSON(rawURL string, body any, headers map[string]string, timeout time.Duration) (string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTPError: %d %s", resp.StatusCode, trunc(string(raw), 200))
	}
	return string(raw), nil
}

// unwrap membuang trailer `data: [DONE]` yang kadang ditempel gateway walau non-streaming.
func unwrap(raw string) string {
	return strings.TrimSpace(strings.SplitN(raw, "data: [DONE]", 2)[0])
}

type chatChoice struct {
	Message struct {
		Content          *string `json:"content"`
		ReasoningContent *string `json:"reasoning_content"`
		Reasoning        *string `json:"reasoning"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

// chat memanggil {GW}/chat/completions TANPA flag `stream`; retry 3x jeda 2 detik.
// Isi jawaban: content -> reasoning_content -> reasoning (beberapa model kn/*
// menaruh hasil di `reasoning` dengan finish_reason=length).
func chat(model, prompt, system string, maxTokens int, timeout time.Duration) (string, error) {
	msgs := []map[string]string{}
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": prompt})
	body := map[string]any{"model": model, "messages": msgs, "max_tokens": maxTokens}

	last := ""
	for attempt := 0; attempt < 3; attempt++ {
		raw, err := postJSON(GW+"/chat/completions", body,
			map[string]string{"Authorization": "Bearer " + key()}, timeout)
		if err == nil {
			var d chatResponse
			if err = json.Unmarshal([]byte(unwrap(raw)), &d); err == nil {
				if len(d.Choices) == 0 {
					last = "kosong (choices)"
				} else {
					m := d.Choices[0].Message
					out := deref(m.Content)
					if strings.TrimSpace(out) == "" {
						out = deref(m.ReasoningContent)
					}
					if strings.TrimSpace(out) == "" {
						out = deref(m.Reasoning)
					}
					if strings.TrimSpace(out) != "" {
						return out, nil
					}
					last = fmt.Sprintf("kosong (finish=%s)", d.Choices[0].FinishReason)
				}
			} else {
				last = err.Error()
			}
		} else {
			last = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("RuntimeError: %s: %s", model, last)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var fenceRe = regexp.MustCompile("```(?:json)?")

// firstJSON mem-parsing JSON pertama di dalam teks (tahan ```fence, prosa,
// dan kurung bersarang di dalam string).
func firstJSON(text string) any {
	if text == "" {
		return nil
	}
	text = fenceRe.ReplaceAllString(text, "")
	for i := range len(text) {
		if text[i] != '{' && text[i] != '[' {
			continue
		}
		if v, ok := decodeFirst(text[i:]); ok {
			return v
		}
	}
	return nil
}

func decodeFirst(b string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(b))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

func jget(rawURL string, params url.Values, timeout time.Duration) (any, error) {
	u := rawURL
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTPError: %d %s", resp.StatusCode, trunc(string(b), 200))
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}

var (
	tagRe   = regexp.MustCompile(`<[^>]+>`)
	spaceRe = regexp.MustCompile(`\s+`)
	sentRe  = regexp.MustCompile(`[.؟!۔]|\n`)
)

func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = tagRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

func sentences(ar string) int {
	n := 0
	for _, s := range sentRe.Split(ar, -1) {
		if utf8.RuneCountInString(strings.TrimSpace(s)) > 8 {
			n++
		}
	}
	return n
}

// pyStr meniru f-string Python: None untuk null, angka JSON tetap terbaca.
func pyStr(v any) string {
	if v == nil {
		return "None"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func asString(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func listStrings(v any) []string {
	out := []string{}
	for _, it := range asList(v) {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// asIdx validasi idx gaya Python: harus int 0 <= i < n, selain itu -1.
func asIdx(v any, n int) int {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f < 0 || f >= float64(n) {
		return -1
	}
	return int(f)
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

// marshalJSON tanpa escaping HTML (setara json.dumps ensure_ascii=False).
func marshalJSON(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
