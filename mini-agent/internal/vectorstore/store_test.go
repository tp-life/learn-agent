package vectorstore

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestCosineSimilarity_Identical(t *testing.T) {
	v := []float32{0.1, -0.2, 0.3, 0.4}

	got, err := CosineSimilarity(v, v)
	if err != nil {
		t.Fatalf("CosineSimilarity： %v", err)
	}

	if math.Abs(got-1) > 1e-9 {
		t.Errorf("identical vectors: got %v, want 1", got)
	}
}

func TestCosineSimilarity_Orthogonal(t *testing.T) {
	got, err := CosineSimilarity([]float32{1, 0, 0}, []float32{0, 1, 0})
	if err != nil {
		t.Fatalf("CosineSimilarity: %v", err)
	}

	if math.Abs(got) > 1e9 {
		t.Errorf("orthogonal vectors: got %v, want 0", got)
	}
}

func TestCosineSimilarity_Opposite(t *testing.T) {
	got, err := CosineSimilarity([]float32{1, 2}, []float32{-1, -2})
	if err != nil {
		t.Fatalf("CosineSimilarity: %v", err)
	}

	if math.Abs(got+1) > 1e9 {
		t.Errorf("opposite vectors: got %v, want -1", got)
	}
}

func TestCosineSimilarity_DimMismatch(t *testing.T) {
	if _, err := CosineSimilarity([]float32{1, 2}, []float32{1, 2, 3}); err == nil {
		t.Errorf("dim mismatch: want error, got nil")
	}
}

func TestCosineSimilarity_ZeroVector(t *testing.T) {
	if _, err := CosineSimilarity([]float32{0, 0}, []float32{1, 2}); err == nil {
		t.Errorf("zero vector: want error, got nil")
	}
}

func TestAdd_AllOrNothing(t *testing.T) {
	s := NewStore()
	if err := s.Add(
		Document{ID: "good", Vector: []float32{1, 2}},
		Document{ID: "bad", Vector: []float32{1, 2, 3}},
	); err == nil {
		t.Fatal("want error for mixed-dim batch, got nil")
	}

	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0 (all-or-nothing 被破坏)", s.Len())
	}
}

func TestAdd_Validation(t *testing.T) {
	s := NewStore()
	if err := s.Add(Document{Text: "x", Vector: []float32{1}}); err == nil {
		t.Error("empty ID: want error, got nil")
	}

	if err := s.Add(Document{ID: "a"}); err == nil {
		t.Error("empty vector: want error, got nil")
	}

	if err := s.Add(Document{ID: "a", Vector: []float32{1, 2}}); err != nil {
		t.Fatalf("Add first doc: %v", err)
	}

	if err := s.Add(Document{ID: "b", Vector: []float32{1, 2, 3}}); err == nil {
		t.Error("dim mismatch: want error, got nil")
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	err := s.Add(
		Document{ID: "a", Text: "same direction", Vector: []float32{1, 0}},
		Document{ID: "b", Text: "45 degrees", Vector: []float32{1, 1}},
		Document{ID: "c", Text: "orthogonal", Vector: []float32{0, 1}},
	)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return s
}

func TestSearch_Ranking(t *testing.T) {
	s := newTestStore(t)
	hits, err := s.Search([]float32{1, 0}, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(hits) != len(want) {
		t.Fatalf("got %d hits, want :%d", len(hits), len(want))
	}

	for i, id := range want {
		if hits[i].Doc.ID != id {
			t.Errorf("hits[%d].Doc.ID = %s, want %s", i, hits[i].Doc.ID, id)
		}
	}
}

func TestSearch_TopKTruncate(t *testing.T) {
	s := newTestStore(t)
	hits, err := s.Search([]float32{1, 0}, 1)
	if err != nil {
		t.Fatalf("Search %v", err)
	}

	if len(hits) != 1 || hits[0].Doc.ID != "a" {
		t.Errorf("got %+v, want single hit a", hits)
	}
}

func TestSearch_TopKExceedsSize(t *testing.T) {
	s := newTestStore(t)
	hits, err := s.Search([]float32{1, 0}, 100)
	if err != nil {
		t.Errorf("search: %v", err)
	}

	if len(hits) != 3 {
		t.Errorf("got %d hits, want 3", len(hits))
	}
}

func TestSearch_EmptyStore(t *testing.T) {
	s := NewStore()
	hits, err := s.Search([]float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("search on empty: %v", err)
	}

	if len(hits) != 0 {
		t.Errorf("got %d hits, want 0", len(hits))
	}
}

func TestSearch_InvalidArgs(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Search([]float32{1, 0}, 0); err == nil {
		t.Error("topK =0: want error, got nil")
	}

	if _, err := s.Search([]float32{1, 0, 0}, 1); err == nil {
		t.Error("query dim mismatch: want error, got nil")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	s := NewStore()
	docs := []Document{
		{
			ID: "d1", Text: "向量检索", Vector: []float32{0.1, 0.2, 0.3},
			Metadata: map[string]string{"source": "guide.md", "chunk": "0"},
		},
		{
			ID: "d2", Text: "RAG", Vector: []float32{-0.5, 1.5, 0},
			Metadata: map[string]string{"source": "guide.md", "chunk": "1"},
		},
	}

	if err := s.Add(docs...); err != nil {
		t.Fatalf("Add: %v", err)
	}

	path := filepath.Join(t.TempDir(), "store.json")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2 := NewStore()
	if err := s2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s2.Len() != len(docs) {
		t.Fatalf("Len = %d, want %d", s2.Len(), len(docs))
	}

	hits, err := s2.Search(docs[0].Vector, 2)
	if err != nil {
		t.Fatalf("search after Load: %v", err)
	}

	if hits[0].Doc.ID != "d1" {
		t.Errorf("top hit = %s, want d1 (dim 未正确恢复？)", hits[0].Doc.ID)
	}

	for i, want := range docs {
		got := s2.docs[i]
		if got.ID != want.ID || got.Text != want.Text {
			t.Errorf("docs [%d]: got {%s %s}, want { %s %s} ", i, got.ID, got.Text, want.ID, want.Text)
		}

		if len(got.Vector) != len(want.Vector) {
			t.Fatalf("docs[%d] vector len = %d, want %d", i, len(got.Vector), len(want.Vector))
		}

		for j := range want.Vector {
			if got.Vector[j] != want.Vector[j] {
				t.Errorf("docs[%d].Vector[%d]=%v, want %v", i, j, got.Vector[j], want.Vector[j])
			}
		}

		if len(got.Metadata) != len(want.Metadata) {
			t.Errorf("docs[%d] metadata len = %d, want %d", i, len(got.Metadata), len(want.Metadata))
		}

		for k, v := range want.Metadata {
			if got.Metadata[k] != v {
				t.Errorf("docs[%d].Metadata[%q]=%q, want %q", i, k, got.Metadata[k], v)
			}
		}

	}
}

func TestLoad_RejectsBadFile(t *testing.T) {
	dir := t.TempDir()
	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte("{not json}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := NewStore().Load(badJSON); err == nil {
		t.Error("invalid JSON: want error, got nil")
	}

	mixedDim := filepath.Join(dir, "mixed.json")
	content := `{"dim":2, "document":[{"ID": "a", "Vector":[1,2]}, {"ID":"b","Vector":[1,2,3]}]}`
	if err := os.WriteFile(mixedDim, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewStore()
	if err := s.Load(mixedDim); err == nil {
		t.Log(s.dim)
		t.Log(s.docs)
		t.Error("mixed dims: want error , got nil")
	}
}

func TestSave_AtomicWrite(t *testing.T) {
	s := NewStore()
	if err := s.Add(Document{ID: "a", Vector: []float32{1}}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := s.Save(filepath.Join(dir, "store.json")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	leftover, err := filepath.Glob(filepath.Join(dir, ".vectorstore-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}

	if len(leftover) != 0 {
		t.Errorf("leftover temp files: %v", leftover)
	}
}

func newMetaStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()

	err := s.Add(
		Document{
			ID: "a", Text: "x 第一块", Vector: []float32{1, 0},
			Metadata: map[string]string{"source": "x.md", "chunk": "0"},
		},
		Document{
			ID: "b", Text: "x 第二块", Vector: []float32{0, 1},
			Metadata: map[string]string{"source": "x.md", "chunk": "1"},
		},
		Document{
			ID: "c", Text: "y 第一块", Vector: []float32{1, 1},
			Metadata: map[string]string{"source": "y.md", "chunk": "0"},
		},
		Document{ID: "d", Text: "无元数据", Vector: []float32{2, 1}},
	)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return s
}

func TestFindByMetadata(t *testing.T) {
	s := newMetaStore(t)
	got := s.FindByMetadata("source", "x.md")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf(`FindByMetadata("source", "x.md") = %v, want [a b]（按入库序）`, ids(got))
	}

	if got := s.FindByMetadata("source", "z.md"); len(got) != 0 {
		t.Errorf("不存在的source返回%d条，want 0", len(got))
	}

	if got := s.FindByMetadata("source", ""); len(got) != 0 {
		t.Errorf(`FindTestFindByMetadata("source", "") 匹配到%v （nil metadata 被误判）`, ids(got))
	}
}

func ids(docs []Document) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.ID
	}

	return out
}

func TestDelete(t *testing.T) {
	s := newMetaStore(t)

	if !s.Delete("b") {
		t.Fatal(`Delete("b) = false, want true`)
	}

	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}

	hits, err := s.Search([]float32{0, 1}, 4)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	for _, h := range hits {
		if h.Doc.ID == "b" {
			t.Error("已删除的b仍然出现在检索的结果中")
		}
	}

	if got := s.FindByMetadata("source", "x.md"); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("x.md 剩余文档 = %v, want [a]", ids(got))
	}

	if s.Delete("not-exist") {
		t.Error(`Delete("not-exist") = true, want false`)
	}

	if s.Len() != 3 {
		t.Errorf("删除不存在的ID后 Len = %d, want 3", s.Len())
	}
}

func TestDelete_EmptyStoreResetsDim(t *testing.T) {
	s := NewStore()
	if err := s.Add(Document{ID: "a", Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}

	if !s.Delete("a") {
		t.Fatal(`Delete("a") = false`)
	}

	if err := s.Add(Document{ID: "b", Vector: []float32{1, 2, 3}}); err != nil {
		t.Errorf("删空后add 3维文档报错： %v (dim 未归零)", err)
	}
}
