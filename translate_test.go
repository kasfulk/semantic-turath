package main

import "testing"

// Model kn-b/deepseek-v4-1-flash kadang membalas objek {"results":[{id,quote,...}]}
// alih-alih array [{"idx","terjemahan"}]. Parser harus tahan kedua bentuk.
func TestExtractTranslatedShapes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"array langsung", []any{
			map[string]any{"idx": float64(0), "terjemahan": "a"},
			map[string]any{"idx": float64(1), "terjemahan": "b"},
		}, 2},
		{"objek results id/quote", map[string]any{"results": []any{
			map[string]any{"id": float64(0), "quote": "a"},
			map[string]any{"id": float64(2), "translation": "c"},
		}}, 2},
		{"objek data", map[string]any{"data": []any{
			map[string]any{"nomor": "1", "teks": "x"},
		}}, 1},
		{"objek tunggal", map[string]any{"idx": float64(0), "terjemahan": "a"}, 1},
		{"tak dikenal", "bukan json terjemahan", 0},
	}
	for _, c := range cases {
		if got := len(extractTranslated(c.in)); got != c.want {
			t.Errorf("%s: extractTranslated = %d item, mau %d", c.name, got, c.want)
		}
	}
}

func TestItemIdxAndText(t *testing.T) {
	m := map[string]any{"id": "2", "translation": "  hasil  "}
	idx, ok := itemIdx(m, 5)
	if !ok || idx != 2 {
		t.Fatalf("itemIdx string id = %d %v", idx, ok)
	}
	if got := itemText(m); got != "hasil" {
		t.Fatalf("itemText = %q", got)
	}
	if got := extractPageText(`{"meta":"{}","text":"isi halaman"}`); got != "isi halaman" {
		t.Fatalf("extractPageText json = %q", got)
	}
	if got := extractPageText("teks mentah"); got != "teks mentah" {
		t.Fatalf("extractPageText raw = %q", got)
	}
	if p, ok := asInt("247"); !ok || p != 247 {
		t.Fatalf("asInt string = %d %v", p, ok)
	}
	if _, ok := asInt("abc"); ok {
		t.Fatal("asInt harus menolak non-angka")
	}
	if _, ok := itemIdx(map[string]any{"idx": float64(9)}, 5); ok {
		t.Fatal("idx di luar rentang harus ditolak")
	}
}

func TestNormArabicAndSamples(t *testing.T) {
	got := normArabic("<span>الصَّلَاةُ</span> الوسطى ـــ وصلاة العصر")
	if got != "الصلاة الوسطى وصلاة العصر" {
		t.Fatalf("normArabic = %q", got)
	}
	s := samplesNukilan("a b c d e f g h i j k l m n o p q r s t")
	if len(s) != 3 {
		t.Fatalf("samplesNukilan = %d potongan, mau 3", len(s))
	}
	for _, x := range s {
		if len(splitFields(x)) != 8 {
			t.Fatalf("potongan bukan 8 kata: %q", x)
		}
	}
	if len(samplesNukilan("dua kata saja")) != 1 {
		t.Fatal("nukilan pendek harus jadi satu potongan")
	}
}

func splitFields(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
