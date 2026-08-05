package tools

import "testing"

func TestCalculator(t *testing.T) {
	c := Calculator{}

	got, err := c.Execute(`{"expression": "(3+4)*2"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "14" {
		t.Fatalf("want 14, got %q", got)
	}
}

func TestCalculatorRejectsUnsafeExpr(t *testing.T) {
	c := Calculator{}
	// 非常量表达式（函数调用）应被拒绝，这是安全边界
	if _, err := c.Execute(`{"expression": "println(1)"}`); err == nil {
		t.Fatal("expected error for non-constant expression")
	}
}

func TestRegistryUnknownTool(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Call("not_exist", `{}`); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}
