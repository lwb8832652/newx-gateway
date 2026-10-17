package registry

import (
	"strings"
	"testing"

	"newx-gateway/internal/config"
)

func TestNewParsesSpecs(t *testing.T) {
	reg, err := New([]Spec{
		{Code: "huobao", BaseURL: "http://127.0.0.1:5679"},
		{Code: "novel", BaseURL: " http://127.0.0.1:8082 "},
		{Code: "commerce"},
	})
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}

	if reg.Len() != 3 {
		t.Fatalf("Len = %d，期望 3", reg.Len())
	}
	// 地址两端空格应被裁剪。
	novel, ok := reg.Lookup("novel")
	if !ok || novel.BaseURL != "http://127.0.0.1:8082" {
		t.Errorf("novel 记录异常: %+v, ok=%v", novel, ok)
	}
	// 未给出地址的应用仍然可注册，仅表示下游尚未接入。
	commerce, ok := reg.Lookup("commerce")
	if !ok || commerce.BaseURL != "" {
		t.Errorf("commerce 记录异常: %+v, ok=%v", commerce, ok)
	}
	if !equalStrings(reg.Codes(), []string{"huobao", "novel", "commerce"}) {
		t.Errorf("Codes 顺序异常: %v", reg.Codes())
	}
}

func TestNewRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		specs  []Spec
		expect string
	}{
		{"空表", nil, "不能为空"},
		{"编码格式错误", []Spec{{Code: "Bad_Code"}}, "应用编码非法"},
		{"编码大写", []Spec{{Code: "Huobao"}}, "应用编码非法"},
		{"编码过短", []Spec{{Code: "a"}}, "应用编码非法"},
		{"重复编码", []Spec{{Code: "huobao"}, {Code: "huobao"}}, "重复"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.specs)
			if err == nil {
				t.Fatal("期望报错，实际构建成功")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.expect)
			}
		})
	}
}

func TestLookupMiss(t *testing.T) {
	reg, err := New([]Spec{{Code: "huobao", BaseURL: "http://127.0.0.1:5679"}})
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}

	if _, ok := reg.Lookup("unknown"); ok {
		t.Error("未注册的应用不应命中")
	}
	if _, ok := reg.Lookup("  "); ok {
		t.Error("空编码不应命中")
	}
	if app, ok := reg.Lookup(" huobao "); !ok || app.Code != "huobao" {
		t.Error("查询应忽略两端空格")
	}
}

func TestAllReturnsCopy(t *testing.T) {
	reg, err := New([]Spec{{Code: "huobao"}})
	if err != nil {
		t.Fatalf("构建注册表失败: %v", err)
	}

	all := reg.All()
	all[0].Code = "mutated"
	if again := reg.All(); again[0].Code != "huobao" {
		t.Error("All 应返回副本，外部修改不得影响注册表")
	}

	codes := reg.Codes()
	codes[0] = "mutated"
	if again := reg.Codes(); again[0] != "huobao" {
		t.Error("Codes 应返回副本，外部修改不得影响注册表")
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	var reg *Registry
	if _, ok := reg.Lookup("huobao"); ok {
		t.Error("nil 注册表查询应返回未命中")
	}
	if reg.Len() != 0 {
		t.Error("nil 注册表长度应为 0")
	}
	if reg.All() != nil || reg.Codes() != nil {
		t.Error("nil 注册表应返回 nil 切片")
	}
}

func TestSpecsFromConfig(t *testing.T) {
	specs := SpecsFromConfig([]config.AppConfig{{Code: "huobao", BaseURL: "http://x:1"}})
	if len(specs) != 1 || specs[0].Code != "huobao" || specs[0].BaseURL != "http://x:1" {
		t.Errorf("转换结果异常: %+v", specs)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
