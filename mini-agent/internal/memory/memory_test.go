package memory

import (
	"mini-agent/internal/vectorstore"
	"path/filepath"
	"strings"
	"testing"
)

type fakeEmbedder struct {
	vecs       map[string][]float32
	defaultVec []float32
}

func (f fakeEmbedder) Embed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := f.vecs[t]
		if !ok {
			v = f.defaultVec
		}
		out[i] = v
	}
	return out, nil
}

func newTestStore(t *testing.T, path string) *Store {
	t.Helper()
	emb := fakeEmbedder{
		vecs: map[string][]float32{
			"用户不吃辣":   {1, 0},
			"用户的猫叫年糕": {0, 1},
			"饮食偏好":    {1, 0.1},
		},
		defaultVec: []float32{0.5, 0.5},
	}
	return NewStore(vectorstore.NewStore(), emb, path)
}

func TestRememberThenRecall_SemanticHit(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	if err := s.Remember("用户的猫叫年糕"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	facts, err := s.Recall("饮食偏好", 1)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if len(facts) != 1 || facts[0] != "用户不吃辣" {
		t.Errorf("Recall = %v, want [用户不吃辣]（语义命中失败）", facts)
	}
}

func TestRemember_RejectsEmpty(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	if err := s.Remember("  "); err == nil {
		t.Error("blank fact: want error, got nil")
	}
}

func TestRecall_EmptyStore(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	facts, err := s.Recall("随便查点什么", 3)
	if err != nil {
		t.Fatalf("Recall on empty store: %v", err)
	}

	if len(facts) != 0 {
		t.Errorf("got %d facts, want 0", len(facts))
	}
}

func TestRecall_TopKDefault(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Recall("饮食偏好", 0); err != nil {
		t.Errorf("topK = 0; want no error (default kicks in), got %v", err)
	}
}

func TestPersistence_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")

	s1 := newTestStore(t, path)
	if err := s1.Remember("用户不吃辣"); err != nil {
		t.Fatalf("Remember : %v", err)
	}

	vs2 := vectorstore.NewStore()
	if err := vs2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	s2 := NewStore(vs2, s1.emb, path)
	facts, err := s2.Recall("饮食偏好", 1)
	if err != nil {
		t.Fatalf("Recall after reload: %v", err)
	}

	if len(facts) != 1 || facts[0] != "用户不吃辣" {
		t.Errorf("Recall after reload = %v, want [用户不吃辣]", facts)
	}
}

func TestMemorySaveTool_Execute(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	tool := MemorySave{Store: s}

	out, err := tool.Execute(`{"fact": "用户不吃辣"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(out, "用户不吃辣") {
		t.Errorf("output %q does not confirm the fact", out)
	}

	facts, err := s.Recall("饮食偏好", 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(facts) != 1 || facts[0] != "用户不吃辣" {
		t.Errorf("fact not in store after tool Execute : %v", facts)
	}
}

func TestMemoryRecallTool_Execute(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	tool := MemoryRecall{Store: s}

	out, err := tool.Execute(`{"query": "饮食偏好"}`)
	if err != nil {
		t.Fatalf("Execute on empty: %v", err)
	}

	if !strings.Contains(out, "没有找到相关记忆") {
		t.Errorf("empty store output %q, want 明确的否定结果", out)
	}

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}

	out, err = tool.Execute(`{"query": "饮食偏好", "top_k": 1}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "用户不吃辣") {
		t.Errorf("output %q missing the fact", out)
	}
}
