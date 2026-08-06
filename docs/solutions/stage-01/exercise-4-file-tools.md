# 练习 4 参考答案：文件读写工具（安全边界设计）

> 对应题目：`mini-agent/internal/tools/tools.go` 末尾 TODO(练习4)
> ⚠️ 先自己实现，再对照本文档。

## 参考实现

```go
// file.go（对应 TODO 要求的文件名）
package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FileTool 把文件读写限制在 Root 目录内。
// 这是"路径穿越防护"的经典场景：模型可能被 prompt 注入诱导
// 去读 ../../etc/passwd，防线必须在我们的代码里，而不是指望模型自觉。
type FileTool struct {
	Root    string // 允许访问的根目录
	MaxRead int64  // 读文件截断上限
}

// resolve 把模型给的相对路径解析成安全的目标路径。
// 所有文件操作都必须经过这一关。
func (f FileTool) resolve(p string) (string, error) {
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("不允许绝对路径: %q", p)
	}
	root := filepath.Clean(f.Root)
	full := filepath.Clean(filepath.Join(root, p))
	// Clean 后的路径必须仍等于 root 或位于 root 之下；
	// "../x" 经 Join+Clean 后会跑到 root 外面，在这里被拦下
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径越出工作目录: %q", p)
	}
	return full, nil
}

// ---- read_file ----

type readFileTool struct{ FileTool }

func NewReadFile(root string) Tool { return readFileTool{FileTool{Root: root, MaxRead: 8000}} }

func (t readFileTool) Name() string { return "read_file" }

func (t readFileTool) Description() string {
	return "读取工作目录内指定文件的内容（超长会截断）。当需要查看已有文件时使用；写文件请用 write_file。"
}

func (t readFileTool) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "相对工作目录的文件路径，如 notes/todo.txt",
			},
		},
		"required": []string{"path"},
	}
}

func (t readFileTool) Execute(args string) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return "", err
	}
	full, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(full)
	if err != nil {
		return "", fmt.Errorf("open: %w", err) // 文件不存在也把错误喂回模型，它会换路径重试
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, t.MaxRead))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ---- write_file ----

type writeFileTool struct{ FileTool }

func NewWriteFile(root string) Tool { return writeFileTool{FileTool{Root: root}} }

func (t writeFileTool) Name() string { return "write_file" }

func (t writeFileTool) Description() string {
	return "把内容写入工作目录内的文件：不存在则创建（含父目录），已存在则覆盖。当需要新建或整体替换文件时使用；只想查看内容请用 read_file。"
}

func (t writeFileTool) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "相对工作目录的文件路径"},
			"content": map[string]any{"type": "string", "description": "要写入的完整内容"},
		},
		"required": []string{"path", "content"},
	}
}

func (t writeFileTool) Execute(args string) (string, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return "", err
	}
	full, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(full, []byte(p.Content), 0o644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return fmt.Sprintf("已写入 %s（%d 字节）", p.Path, len(p.Content)), nil
}
```

main.go 注册（工作目录限定为 `./workspace`）：

```go
registry.Register(tools.NewReadFile("./workspace"))
registry.Register(tools.NewWriteFile("./workspace"))
```

## 关键设计点

1. **防线在代码，不在模型**：防路径穿越不能靠 prompt 里写"不要访问敏感文件"——prompt 注入可以绕过模型层的任何"自觉"。`resolve` 是唯一可信的防线，所有文件操作强制经过它。
2. **`filepath.Clean` 之后再判断前缀**：`Join(root, "../../etc/passwd")` 经 Clean 后，root 为相对路径（如 `./workspace`）时变成 `../etc/passwd`，root 为绝对路径时变成 `/etc/passwd`——两种情况的共同点是**都不再落在 root 前缀内**，前缀比对直接拦下。不做 Clean 就比对是经典漏洞写法（`a/../..` 之类能绕过）。
3. **绝对路径直接拒绝**：相对路径语义让"工作目录"这个边界清晰可守，也少一类绕过手段。
4. **写工具的语义要在 Description 里讲死**："覆盖还是追加""是否建父目录"必须明确，否则模型会在拿不准时乱猜（比如以为 write 是追加）。
5. **写结果回执要具体**：返回"已写入 X（N 字节）"而不是"成功"，模型后续轮次能据此向用户汇报，也能自己 sanity check。
6. **（进阶）还能加什么**：禁止符号链接逃逸（`filepath.EvalSymlinks` 后再校验）、文件大小上限、禁止写可执行后缀。面试能聊到这一层就是加分项。

## 对照清单

- [x] 读、写两个工具都实现了 Tool 接口并注册
- [x] 所有路径经过统一的 resolve/校验函数，拒绝绝对路径
- [x] 用 `filepath.Clean` + 前缀判断防 `../` 逃逸
- [x] 手动验证 `../../etc/passwd` 被拒绝（必须真的试过）
- [x] read_file 有返回长度截断
- [x] write_file 的覆盖/建目录语义写进了 Description
- [x] 两个工具的 Description 写清了各自的使用时机（读用谁、写用谁）
- [x] 端到端验证：agent 能完成"把计算结果写入文件再读回来"
