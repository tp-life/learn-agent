package tools

import (
	"fmt"
	"go/token"
	"go/types"
)

// Calculator 用 Go 自己的类型系统做安全的表达式求值，
// 避免引入第三方库，也避免 eval 任意代码的风险。
//
// 练习：本文件无需学习者完成的部分（练习 4 文件工具见 tools.go 末尾）。
type Calculator struct{}

func (Calculator) Name() string { return "calculator" }

func (Calculator) Description() string {
	return "计算数学表达式。当需要精确算术（加减乘除、括号、百分比等）时使用，不要心算。"
}

func (Calculator) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"expression": map[string]any{
				"type":        "string",
				"description": "数学表达式，如 (3+4)*2/5",
			},
		},
		"required": []string{"expression"},
	}
}

func (Calculator) Execute(args string) (string, error) {
	var p struct {
		Expression string `json:"expression"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return "", err
	}

	fmt.Println("这里是实际调用的地方：", args)

	// types.Eval 只支持常量表达式，天然安全（无法调用函数或访问变量）
	tv, err := types.Eval(token.NewFileSet(), nil, token.NoPos, p.Expression)
	if err != nil {
		return "", fmt.Errorf("invalid or unsafe expression: %w", err)
	}
	if tv.Value == nil {
		// 例如 println(1) 这类无值的内置调用，Eval 不报错但结果是空的
		return "", fmt.Errorf("expression has no value: %q", p.Expression)
	}
	return tv.Value.String(), nil
}
