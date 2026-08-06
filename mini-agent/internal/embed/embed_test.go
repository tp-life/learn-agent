// Package embed 封装 OpenAI 兼容的 embedding API 客户端（默认指向硅基流动）。
//
// 在 agent 链路中的位置：这是 RAG 的"翻译器"——把文本翻译成向量，
// 下游的向量库（internal/vectorstore，练习 2）和 RAG 工具（练习 4）都依赖它。
//
// 为什么单独建包而不是复用 internal/llm：
// DeepSeek 官方没有 embedding API，embedding 必须走另一家服务商
// （硅基流动 / 本地 Ollama），baseURL、apiKey、模型名、端点（/embeddings）
// 全都不同，硬塞进 llm.Client 只会让它背两套配置。
// 两者相同的是"OpenAI 兼容协议"这个外壳：同样的 Authorization 头、
// 同样的错误处理模式（APIError）、同样的重试需求。
package embed

import (
	"encoding/json"
	"errors"
	"fmt"
	"mini-agent/internal/llm"
	"net/http"
	"net/http/httptest"
	"testing"
)

func makeVec(tag float32) []float32 {
	v := make([]float32, BgeM3Dimensions)
	v[0] = tag
	return v
}

func newFakeServer(t *testing.T, status int, jsonBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		if r.Header.Get("Authorization") == "" {
			t.Errorf("missing Authorization header")
		}

		w.WriteHeader(status)
		fmt.Fprint(w, jsonBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbed_ReordersByIndex(t *testing.T) {
	texts := []string{"alpha", "beta", "gamma"}

	resp := embeddingResponse{}
	resp.Data = append(resp.Data,
		struct {
			Index     int       "json:\"index\""
			Embedding []float32 "json:\"embedding\""
		}{
			Index: 2, Embedding: makeVec(2),
		},
		struct {
			Index     int       "json:\"index\""
			Embedding []float32 "json:\"embedding\""
		}{
			Index: 0, Embedding: makeVec(0),
		},
		struct {
			Index     int       "json:\"index\""
			Embedding []float32 "json:\"embedding\""
		}{
			Index: 1, Embedding: makeVec(1),
		},
	)

	body, _ := json.Marshal(resp)
	srv := newFakeServer(t, http.StatusOK, string(body))

	c := NewClient("fake-key").WithBaseURL(srv.URL)
	vecs, err := c.Embed(texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	for i, v := range vecs {
		if v[0] != float32(i) {
			t.Errorf("vecs [%d][0] = %v, want %d (归位错误)", i, v[0], i)
		}
	}
}

func TestEmbed_RejectsEmptyInput(t *testing.T) {
	c := NewClient("fake-key").WithBaseURL("http://127.0.0.1:1")
	if _, err := c.Embed(nil); err == nil {
		t.Error("empty slice: want error, got nil")
	}

	if _, err := c.Embed([]string{"ok", "  "}); err == nil {
		t.Error("blank string: want error, got nil")
	}
}

func TestEmbed_WrongDimension(t *testing.T) {
	body := `{"data": [{"index": 0, "embedding":[0.1,0.2,0.3]}]}`
	srv := newFakeServer(t, http.StatusOK, body)

	c := NewClient("fake-key").WithBaseURL(srv.URL)

	if _, err := c.Embed([]string{"hello"}); err == nil {
		t.Error("want dimension error, got nil")
	}
}

func TestEmbed_Non200ReturnsAPIError(t *testing.T) {
	srv := newFakeServer(t, http.StatusUnauthorized, `{"error": "invalid key"}`)
	c := NewClient("bad-key").WithBaseURL(srv.URL)

	_, err := c.Embed([]string{"hello"})

	var apiErr *llm.APIError

	if !errors.As(err, &apiErr) {
		t.Fatalf("want *llm.APIError, got %T: %v", err, err)
	}

	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
}
