package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type FileTool struct {
	Root    string
	MaxRead int64
}

func (f FileTool) resolve(p string) (string, error) {
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("不允许绝对路径：%q", p)
	}

	root := filepath.Clean(f.Root)
	full := filepath.Clean(filepath.Join(root, p))

	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径越出工作目录 : %q", p)
	}

	return full, nil
}

type readFileTool struct{ FileTool }

func NewReadFile(root string) Tool {
	return readFileTool{FileTool{Root: root, MaxRead: 8000}}
}

func (t readFileTool) Name() string { return "read_file" }

func (t readFileTool) Description() string {
	return `读取工作目录内指定文件的内容（超长会截断）。
	当需要查看已有文件时使用；写文件请用write_file。`
}

func (t readFileTool) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "相对工作目录的文件路径， 如 notes/todo.txt",
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
		return "", fmt.Errorf("open: %w", err)
	}

	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, t.MaxRead))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type writeFileTool struct{ FileTool }

func NewWriteFile(root string) Tool {
	return writeFileTool{FileTool{Root: root}}
}

func (t writeFileTool) Name() string { return "write_file" }

func (t writeFileTool) Description() string {
	return "把内容写入工作目录内的文件： 不存在则创建（含父目录），已存在则覆盖。 当需要新建或整体替换文件时使用；只想查看内容请用 read_file。"
}

func (t writeFileTool) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "相对工作目录的文件路径",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "要写入的完整内容",
			},
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
	return fmt.Sprintf("已写入 %s (%d字节)", p.Path, len(p.Content)), nil
}
