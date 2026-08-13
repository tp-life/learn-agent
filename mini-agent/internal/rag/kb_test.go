package rag

import (
	"fmt"
	"mini-agent/internal/vectorstore"
	"strings"
	"testing"
)

type fakeEmbedder struct {
	vecs map[string][]float32
}

func (f fakeEmbedder) Embed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v, ok := f.vecs[text]
		if !ok {
			return nil, fmt.Errorf("fakeEmbedder: no vectors for %q", text)
		}
		out[i] = v
	}
	return out, nil
}

func TestIngest_ChunkCountAndMetadata(t *testing.T) {
	p1 := strings.Repeat("甲", 60)
	p2 := strings.Repeat("乙", 60)
	text := p1 + "\n\n" + p2

	emb := fakeEmbedder{vecs: map[string][]float32{
		p1: {1, 0},
		p2: {0, 1},
	}}

	store := vectorstore.NewStore()
	kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})

	n, err := kb.Ingest("notes.md", text)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if n != 2 {
		t.Errorf("Ingest 返回 %d块，wnat  2", n)
	}
	if store.Len() != 2 {
		t.Fatalf("store.Len() = %d, want 2", store.Len())
	}

	hits, err := store.Search([]float32{1, 0}, 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}

	first := hits[0].Doc
	if first.ID != "notes.md#0" {
		t.Errorf("ID=%q want %q", first.ID, "notes.md#0")
	}
	if first.Text != p1 {
		t.Errorf("Text 未保真 （首20 rune: %.20q）", first.Text)
	}
	if first.Metadata["source"] != "notes.md" {
		t.Errorf("Metadata[source]=%q, want %q", first.Metadata["source"], "notes.md")
	}
	if first.Metadata["chunk"] != "0" {
		t.Errorf("Metadata[chunk] = %q, want %q", first.Metadata["chunk"], "0")
	}
	if hits[1].Doc.Metadata["chunk"] != "1" {
		t.Errorf("hits[1].Metadata[chunk]=%q, wnat %q", hits[1].Doc.Metadata["chunk"], "1")
	}
}

func TestIngest_RejectsBadInput(t *testing.T) {
	emb := fakeEmbedder{vecs: map[string][]float32{}}
	kb := NewKnowledgeBase(emb, vectorstore.NewStore(), ChunkOptions{})
	if _, err := kb.Ingest(" ", "内容"); err == nil {
		t.Errorf("empty source: want error ,got nil")
	}

	if _, err := kb.Ingest("a.md", ""); err == nil {
		t.Error("empty text: want error, got nil")
	}

	if _, err := kb.Ingest("a.md", "  \n\n\t "); err == nil {
		t.Error("blank text: want error,got nil")
	}

	if kb.Store().Len() != 0 {
		t.Errorf("失败的Ingest 污染了向量库： Lend= %d， want 0", kb.Store().Len())
	}
}

func TestKBSearch_FormatsNumberedHitsWithSource(t *testing.T) {
	p1 := strings.Repeat("苹果", 30)
	p2 := strings.Repeat("香蕉", 30)
	text := p1 + "\n\n" + p2

	emb := fakeEmbedder{vecs: map[string][]float32{
		p1:      {1, 0},
		p2:      {0, 1},
		"苹果是什么": {1, 0},
	}}

	store := vectorstore.NewStore()
	kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})

	if _, err := kb.Ingest("fruits.md", text); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	tool := NewKBSearch(emb, store)
	out, err := tool.Execute(`{"query": "苹果是什么"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, want := range []string{"[1]", "fruits.md", p1} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q: \n %s", want, out)
		}
	}

	if strings.Contains(out, "[2]") {
		t.Errorf("低分块未被过滤 （出现[2]）:\n%s", out)
	}

	if strings.Contains(out, p2) {
		t.Errorf("低分块文本泄漏进输出：\n %s", out)
	}
}

func TestKBSearch_EmptyStore(t *testing.T) {
	tool := NewKBSearch(fakeEmbedder{vecs: map[string][]float32{}}, vectorstore.NewStore())
	out, err := tool.Execute(`{"query": "随便问"}`)
	if err != nil {
		t.Fatalf("Execute on empty store: %v", err)
	}

	if !strings.Contains(out, "知识库当中没有相关内容") {
		t.Errorf("空库文案不符合预期: %q", out)
	}
}

func TestKBSearch_LowScoreFiltered(t *testing.T) {
	doc := "完全无关的文档内容"
	emb := fakeEmbedder{vecs: map[string][]float32{
		doc:  {0, 1},
		"查询": {1, 0},
	}}

	store := vectorstore.NewStore()
	kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})
	if _, err := kb.Ingest("x.md", doc); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	tool := NewKBSearch(emb, store)
	out, err := tool.Execute(`{"query": "查询"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(out, "知识库中没有相关内容") {
		t.Errorf("低分过滤文案不符合预期：%q", out)
	}

	if strings.Contains(out, doc) {
		t.Errorf("无关文档泄漏进输出： %q", out)
	}
}

func TestKBSearch_InvalidArgs(t *testing.T) {
	tool := NewKBSearch(fakeEmbedder{vecs: map[string][]float32{}}, vectorstore.NewStore())
	if _, err := tool.Execute(`{not json}`); err == nil {
		t.Error("malformed JSON: want error, got nil")
	}

	if _, err := tool.Execute(`{"query":"  "}`); err == nil {
		t.Error("blank query: want error, got nil")
	}
}

type countingEmbedder struct {
	vecs  map[string][]float32
	calls int
}

func (c *countingEmbedder) Embed(texts []string) ([][]float32, error) {
	c.calls += len(texts)
	out := make([][]float32, len(texts))

	for i, text := range texts {
		v, ok := c.vecs[text]
		if !ok {
			fmt.Println("########\n", text, c.vecs)
			return nil, fmt.Errorf("countingEmbedder: no vector for %q", text)
		}
		out[i] = v
	}

	return out, nil
}

func TestIngest_RepeatSameSourceIsNoop(t *testing.T) {
	p1 := strings.Repeat("甲", 60)
	p2 := strings.Repeat("乙", 60)
	text := p1 + "\n\n" + p2

	emb := &countingEmbedder{vecs: map[string][]float32{p1: {1, 0}, p2: {0, 1}}}

	store := vectorstore.NewStore()

	kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})

	n, err := kb.Ingest("notes.md", text)
	if err != nil || n != 2 {
		t.Fatalf("首次 Ingest: n=%d, err=%v; want n=2", n, err)
	}

	callsAfterFirst := emb.calls

	n, err = kb.Ingest("notes.md", text)
	if err != nil {
		t.Fatalf("重复 Ingest: %v", text)
	}

	if n != 0 {
		t.Errorf("重复 Ingest 返回 %d, want 0 (内容未变应跳过)", n)
	}

	if store.Len() != 2 {
		t.Errorf("重复 Ingest 后 Len = %d, want 2 (产生了重复块)", store.Len())
	}

	if emb.calls != callsAfterFirst {
		t.Errorf("重复 Ingest 多调了 %d 次 embedding, 去重短路未生效 （应一次 embedding 都不调）", emb.calls-callsAfterFirst)
	}
}

func TestIngest_ModifiedSourceReplacesOld(t *testing.T) {
	p1 := strings.Repeat("甲", 60)
	p2 := strings.Repeat("乙", 60)
	oldText := p1 + "\n\n" + p2
	newText := strings.Repeat("丙", 60)

	emb := &countingEmbedder{vecs: map[string][]float32{
		p1:      {1, 0},
		p2:      {0, 1},
		newText: {1, 1},
	}}

	store := vectorstore.NewStore()
	kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})

	if _, err := kb.Ingest("notes.md", oldText); err != nil {
		t.Fatalf("Ingest v1: %v", err)
	}

	s := emb.vecs[newText]
	t.Log(s)
	n, err := kb.Ingest("notes.md", newText)
	if err != nil {
		t.Fatalf("Ingest v2: %v", err)
	}

	if n != 1 {
		t.Errorf("v2 Ingest 返回 %d, want 1", n)
	}

	if store.Len() != 1 {
		t.Fatalf("替换后 Len = %d, want 1 (旧块未清干净)", store.Len())
	}
	hits, err := store.Search([]float32{1, 1}, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(hits) != 1 || hits[0].Doc.Text != newText {
		t.Errorf("检索结果 = %v, 库里应只剩新版内容", hits)
	}

	if old := store.FindByMetadata("source", "notes.md"); len(old) != 1 || old[0].Text != newText {
		t.Errorf("FindByMetadata 命中%d条， want 1 条新版", len(old))
	}
}

func TestIngest_DifferentSourcesIndependent(t *testing.T) {
	text := "同一段内容，两个来源各自收录"
	emb := &countingEmbedder{vecs: map[string][]float32{text: {1, 0}}}
	store := vectorstore.NewStore()
	kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})

	if _, err := kb.Ingest("a.md", text); err != nil {
		t.Fatal(err)
	}

	if _, err := kb.Ingest("b.md", text); err != nil {
		t.Fatal(err)
	}

	if store.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (不同source 的相同文本被错误去重)", store.Len())
	}

	if n, _ := kb.Ingest("a.md", text); n != 0 {
		t.Errorf("重复a.md返回%d，want 0", n)
	}

	if got := store.FindByMetadata("source", "b.md"); len(got) != 1 || got[0].Text != text {
		t.Errorf("a.md 的重复入库影响了 b.md： %v", got)
	}

	if store.Len() != 2 {
		t.Errorf("最终 Len = %d, want 2", store.Len())
	}
}
