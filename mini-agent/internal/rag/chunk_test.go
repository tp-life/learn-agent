package rag

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func TestChunk_PacksParagraphs(t *testing.T) {
	paras := []string{"第一段：甲乙丙丁", "第二段：戊己庚辛", "第三段：壬葵"}
	text := strings.Join(paras, "\n\n")

	chunks := Chunk(text, ChunkOptions{MaxChars: 100, OverlapChars: 10})
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1 (三段应打包进同一块)", len(chunks))
	}

	if chunks[0] != text {
		t.Errorf("chunks 内容被改动： got %q, want %q", chunks[0], text)
	}
}

func TestChunk_PackingRespectsMaxChars(t *testing.T) {
	p1 := strings.Repeat("甲", 40)
	p2 := strings.Repeat("乙", 70)
	text := p1 + "\n\n" + p2

	chunks := Chunk(text, ChunkOptions{MaxChars: 100, OverlapChars: 10})
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2", len(chunks))
	}

	if chunks[0] != p1 || chunks[1] != p2 {
		t.Errorf("段落被拆散或改序：got %q", chunks)
	}
	t.Log(chunks, runeLen(text), len([]rune(p1)), len(chunks))
	for i, c := range chunks {
		if runeLen(c) > 100 {
			t.Errorf("chunks[%d] = %d rune, 超过 MaxChars=100", i, runeLen(c))
		}
	}
}

func TestChunk_HardCutWithOverlap(t *testing.T) {
	para := strings.Repeat("a", 250)
	opts := ChunkOptions{MaxChars: 100, OverlapChars: 30}
	chunks := Chunk(para, opts)

	if len(chunks) != 4 {
		t.Fatalf("len()chunks = %d, want 4", len(chunks))
	}

	for i, c := range chunks {
		if runeLen(c) > opts.MaxChars {
			t.Errorf("chunks[%d]=%d rune, 超过 maxChars=%d", i, runeLen(c), opts.MaxChars)
		}
	}

	for i := 0; i+1 < len(chunks); i++ {
		cur, next := []rune(chunks[i]), []rune(chunks[i+1])
		ov := opts.OverlapChars

		if string(cur[len(cur)-ov:]) != string(next[:ov]) {
			t.Errorf("chunks[%d] 与 chunks[%d] 之间重叠不正确", i, i+1)
		}
	}

	var sb strings.Builder
	sb.WriteString(chunks[0])

	for _, c := range chunks[1:] {
		sb.WriteString(string([]rune(c)[opts.OverlapChars:]))
	}

	if sb.String() != para {
		t.Errorf("去重叠拼接后 =%d rune, 与原文不一致 （内容未全覆盖）", runeLen(sb.String()))
	}
}

func TestChunk_ChineseNoMojibake(t *testing.T) {
	para := strings.Repeat("人工智能", 40)
	opts := ChunkOptions{MaxChars: 50, OverlapChars: 10}

	chunks := Chunk(para, opts)
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}

	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunks[%d 不是合法 UTF-8 (按byte 切产生了乱码)]", i)
		}

		if runeLen(c) > opts.MaxChars {
			t.Errorf("chunks[%d] = %d rune, 超过 MaxChars=%d", i, runeLen(c), opts.MaxChars)
		}
	}

	var sb strings.Builder
	sb.WriteString(chunks[0])

	for _, c := range chunks[1:] {
		r := []rune(c)
		overlap := opts.OverlapChars
		if len(r) < overlap {
			overlap = len(r)
		}

		sb.WriteString(string(r[overlap:]))
	}
	if sb.String() != para {
		t.Errorf("中文文本去重叠拼接后与原文不一致： got %d rune, want %d", runeLen(sb.String()), runeLen(para))
	}
}

func TestChunk_EmptyAndBlankInput(t *testing.T) {
	if got := Chunk("", DefaultChunkOptions()); got != nil {
		t.Errorf(`Chunk("") = %v, want nil`, got)
	}

	if got := Chunk("  \n\n\t \n\n  ", DefaultChunkOptions()); got != nil {
		t.Errorf(`Chunk(全空白) = %v, wnat nil`, got)
	}
}

func TestChunk_OverlapClampedNoDeadLoop(t *testing.T) {
	para := strings.Repeat("x", 120)
	done := make(chan []string, 1)

	go func() {
		done <- Chunk(para, ChunkOptions{MaxChars: 50, OverlapChars: 50})
	}()

	select {
	case chunks := <-done:
		if len(chunks) == 0 {
			t.Error("overlap 被钳制后仍应产出块")
		}

		for i, c := range chunks {
			if runeLen(c) > 50 {
				t.Errorf("chunks[%d] = %d rune, 超过 MaxChars=50", i, runeLen(c))
			}
		}

	case <-time.After(2 * time.Second):
		t.Fatal("2s 未返回：OverlapChars >= MaxChars 导致死循环")
	}
}

func TestChunk_ZeroOptsUsesDefault(t *testing.T) {
	para := strings.Repeat("哈", 1000)
	chunks := Chunk(para, ChunkOptions{})
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want >=2 (默认 MaxChars=400应切多块)", len(chunks))
	}

	for i, c := range chunks {
		if runeLen(c) > 400 {
			t.Errorf("chunks[%d] = %d rune, 超过默认 MaxChars=400", i, runeLen(c))
		}
	}
}
