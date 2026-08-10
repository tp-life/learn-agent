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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mini-agent/internal/llm"
	"net/http"
	"strings"
	"time"
)

// bge-m3 的输出维度。写死它的意义：入库前校验向量长度，
// 维度错了说明模型/服务商配错了，越早报错越好——
// 否则错误向量悄悄入库，检索结果全错还很难排查。
const BgeM3Dimensions = 1024

// Client 是一个 OpenAI 兼容的 embeddings 客户端。
// 字段与 llm.Client 同构，学习时可以对照着看。
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewClient 默认指向硅基流动（有免费的 bge-m3，注册送额度）。
// apiKey 从环境变量 SILICONFLOW_API_KEY 读入后传入。
func NewClient(apiKey string) *Client {
	return &Client{
		baseURL: "https://api.siliconflow.cn/v1",
		apiKey:  apiKey,
		model:   "BAAI/bge-m3",
		httpClient: &http.Client{
			Timeout: 60 * time.Second, // embedding 是批处理接口，单批文本多时需要余量
		},
	}
}

// WithBaseURL 切换服务商，例如本地 Ollama："http://localhost:11434/v1"。
func (c *Client) WithBaseURL(baseURL string) *Client {
	c.baseURL = baseURL
	return c
}

// WithModel 切换 embedding 模型。
// 注意：换模型 = 换向量空间，已入库的旧向量全部作废，必须重建索引。
func (c *Client) WithModel(model string) *Client {
	c.model = model
	return c
}

// embeddingRequest 是发给 /embeddings 的请求体。
// input 是数组——批量接口，一次请求 embedding 多段文本，
// 比逐段调用省掉大量 HTTP 往返（入库几百个 chunk 时差距明显）。
type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embeddingResponse 是 /embeddings 的响应体。
//
// 本包最核心的坑：data 数组的顺序不能假设与输入顺序一致！
// 每个元素带 index 字段标明它对应 input 的第几段，
// 归位时必须按 index 放，不能按数组下标直接对应。
// （部分服务商会按 token 数等内部策略重排，文档不保证顺序。）
type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// Embed 输入一批文本，返回与之一一对应的向量切片（result[i] 是 texts[i] 的向量）。
// 实现要点见函数内注释；本练习的参考答案在
// docs/solutions/stage-02/exercise-1-embedding-client.md。
func (c *Client) Embed(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("embed: empty input")
	}

	for i, t := range texts {
		if strings.TrimSpace(t) == "" {
			return nil, fmt.Errorf("embed: texts[%d] is empty", i)
		}
	}

	body, err := json.Marshal(embeddingRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("build request : %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request : %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &llm.APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var embResp embeddingResponse
	if err := json.Unmarshal(respBody, &embResp); err != nil {
		return nil, fmt.Errorf("unmarshal response :%w", err)
	}

	if len(embResp.Data) != len(texts) {
		return nil, fmt.Errorf("embed: got %d embeddings for %d texts", len(embResp.Data), len(texts))
	}

	result := make([][]float32, len(texts))

	for _, d := range embResp.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embed: index %d out of range [0,%d]", d.Index, len(texts))
		}

		if len(d.Embedding) != BgeM3Dimensions {
			return nil, fmt.Errorf("embed: text[%d] dim = %d, want %d", d.Index, len(d.Embedding), BgeM3Dimensions)
		}
		result[d.Index] = d.Embedding
	}

	for i, v := range result {
		if v == nil {
			return nil, fmt.Errorf("embed: texts[%d] missing in response", i)
		}
	}

	return result, nil
}
