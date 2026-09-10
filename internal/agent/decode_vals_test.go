package agent

import "testing"

// TestDecodeFieldValsTolerantNumbers 锁死「LLM 用 JSON number 输出数量/单价时
// 整包解析不能失败」——这是「续改不生效」的根因 bug。
func TestDecodeFieldValsTolerantNumbers(t *testing.T) {
	raw := []byte(`{"contractNo":"HT-001","item1Qty":3,"item1Price":65000,"item2Qty":5,"item2Price":7800.5,"amount":193600,"flag":true,"nested":{"a":1},"arr":[1,2],"empty":null,"blank":""}`)
	m, err := decodeFieldVals(raw)
	if err != nil {
		t.Fatalf("decodeFieldVals 不应报错: %v", err)
	}
	want := map[string]string{
		"contractNo": "HT-001",
		"item1Qty":   "3",
		"item1Price": "65000",
		"item2Qty":   "5",
		"item2Price": "7800.5",
		"amount":     "193600",
		"flag":       "true",
	}
	for k, w := range want {
		if got := m[k]; got != w {
			t.Errorf("key %s: got %q want %q", k, got, w)
		}
	}
	// 整型浮点必须收缩成 "5" 而不是 "5.0"
	if v, _ := decodeFieldVals([]byte(`{"q":5.0}`)); v["q"] != "5" {
		t.Errorf("5.0 应收缩为 \"5\", got %q", v["q"])
	}
	// 显式空串是合法信号（LLM 主动清空该字段），必须保留为 ""，供续改合并逻辑区分
	// 「本轮没提到」与「本轮显式清空」。
	if v, ok := m["blank"]; !ok || v != "" {
		t.Errorf("显式空串应保留为 \"\", got %q ok=%v", v, ok)
	}
	// 嵌套结构/null 不是字段值
	for _, k := range []string{"nested", "arr", "empty"} {
		if _, ok := m[k]; ok {
			t.Errorf("非标量 key %s 不应出现在结果里", k)
		}
	}
}

// TestDecodeFieldValsRejectsNonObject 确认真正的坏 JSON 仍然报错（不掩盖问题）。
func TestDecodeFieldValsRejectsNonObject(t *testing.T) {
	if _, err := decodeFieldVals([]byte(`[1,2,3]`)); err == nil {
		t.Error("数组输入应当报错")
	}
}
