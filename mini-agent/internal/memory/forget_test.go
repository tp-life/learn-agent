package memory

import (
	"mini-agent/internal/vectorstore"
	"path/filepath"
	"testing"
)

type countingEmbedder struct {
	vecs       map[string][]float32
	defaultVec []float32
	calls      int
}

func (c *countingEmbedder) Embed(texts []string) ([][]float32, error) {
	c.calls += len(texts)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := c.vecs[t]
		if !ok {
			v = c.defaultVec
		}
		out[i] = v
	}
	return out, nil
}

func newForgetTestStore(t *testing.T, path string) (*Store, *countingEmbedder) {
	t.Helper()
	emb := &countingEmbedder{
		vecs: map[string][]float32{
			"用户不吃辣":   {1, 0},
			"用户的猫叫年糕": {0, 1},
			"饮食偏好":    {1, 0.1},
			"不吃辣":     {1, 0},
		},
		defaultVec: []float32{0.5, 0.5},
	}

	return NewStore(vectorstore.NewStore(), emb, path), emb
}

func TestRemember_ExactDuplicateSkipped(t *testing.T) {
	s, emb := newForgetTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}

	callsAfterFirst := emb.calls
	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}

	if got := len(s.vs.FindByMetadata("kind", "memory")); got != 1 {
		t.Errorf("重复 Remember 后库里有%d条，want 1", got)
	}

	if emb.calls != callsAfterFirst {
		t.Errorf("重复 Remember 多调了 %d次 embedding, 去重短路未生效", emb.calls-callsAfterFirst)
	}
}

func TestRemember_SimilarButDifferentKept(t *testing.T) {
	s, _ := newForgetTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}

	if err := s.Remember("用户现在吃辣了"); err != nil {
		t.Fatal(err)
	}

	if got := len(s.vs.FindByMetadata("kind", "memory")); got != 2 {
		t.Errorf("语义相近但文本不同的两条事实只存了 %d 条，want 2", got)
	}
}

func TestForget_RemovesMostSimilar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	s, _ := newForgetTestStore(t, path)
	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remember("用户的猫叫年糕"); err != nil {
		t.Fatal(err)
	}

	n, err := s.Forget("不吃辣")
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if n != 1 {
		t.Fatalf("Forget 返回%d, want 1", n)
	}

	facts, err := s.Recall("饮食偏好", 3)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range facts {
		if f == "用户不吃辣" {
			t.Errorf("被遗忘的事实仍能被Recall命中： %v", facts)
		}
	}

	if got := len(s.vs.FindByMetadata("kind", "memory")); got != 1 {
		t.Errorf("Forget 后库里剩 %d 条，want 1(误删了其他记忆)", got)
	}

	vs2 := vectorstore.NewStore()
	if err := vs2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	s2 := NewStore(vs2, s.emb, path)
	facts, err = s2.Recall("饮食偏好", 3)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range facts {
		if f == "用户不吃辣" {
			t.Errorf("重启后被遗忘的事实复活了（删除未落盘： %v）", facts)
		}
	}
}

func TestForget_NoSimilarMemoryReturnsZero(t *testing.T) {
	s, _ := newForgetTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}

	n, err := s.Forget("火星气候")
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if n != 0 {
		t.Errorf("Forget 返回%d, want 0 (不相似的记忆不应被删)", n)
	}
	if got := len(s.vs.FindByMetadata("kind", "memory")); got != 1 {
		t.Errorf("不相似的Forget删除了记忆： 剩 %d 条，want 1", got)
	}
}

func TestForget_EdgeCases(t *testing.T) {
	s, emb := newForgetTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	if _, err := s.Forget("  "); err == nil {
		t.Error("blank query: want error, got nil")
	}
	n, err := s.Forget("随便忘点什么")
	if err != nil || n != 0 {
		t.Errorf("空库 Forget = (%d,%v), want (0,nil)", n, err)
	}
	if emb.calls != 0 {
		t.Errorf("空库 Forget 调了 %d 次embedding (应短路)", emb.calls)
	}
}
