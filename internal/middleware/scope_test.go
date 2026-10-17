package middleware

import "testing"

func TestScopeGuardMatch(t *testing.T) {
	guard := NewScopeGuard()
	guard.Allow(
		Rule{Method: "GET", PathPrefix: "/web/", Scope: "huobao.read"},
		Rule{Method: "POST", PathPrefix: "/web/huobao/v1/dramas", Scope: "huobao.write"},
		Rule{PathPrefix: "/admin/", Scope: "huobao.admin"},
	)

	cases := []struct {
		name        string
		method      string
		path        string
		wantScope   string
		wantMatched bool
	}{
		{"命中读规则", "GET", "/web/huobao/v1/dramas", "huobao.read", true},
		{"方法不匹配则跳过首条", "POST", "/web/huobao/v1/dramas", "huobao.write", true},
		{"命中通配方法的规则", "PUT", "/admin/huobao/v1/users", "huobao.admin", true},
		{"路径不匹配任何规则", "POST", "/web/huobao/v1/scenes", "", false},
		{"无关路径", "GET", "/static/a.png", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule, matched := guard.Match(tc.method, tc.path)
			if matched != tc.wantMatched {
				t.Fatalf("matched = %v，期望 %v", matched, tc.wantMatched)
			}
			if matched && rule.Scope != tc.wantScope {
				t.Errorf("scope = %q，期望 %q", rule.Scope, tc.wantScope)
			}
		})
	}
}

func TestScopeGuardRulesAreCopied(t *testing.T) {
	guard := NewScopeGuard()
	guard.Allow(Rule{Scope: "a"})

	rules := guard.Rules()
	rules[0].Scope = "mutated"
	if guard.Rules()[0].Scope != "a" {
		t.Error("Rules 应返回副本，外部修改不得影响守卫")
	}
}

func TestScopeGuardDisabledByDefault(t *testing.T) {
	guard := NewScopeGuard()
	if guard.Enabled() {
		t.Error("默认必须关闭强制校验：身份体系未接入时开启会拒绝全部请求")
	}

	guard.SetEnabled(true)
	if !guard.Enabled() {
		t.Error("SetEnabled(true) 未生效")
	}
}

func TestNilScopeGuardIsSafe(t *testing.T) {
	var guard *ScopeGuard
	if guard.Enabled() {
		t.Error("nil 守卫不应报告已启用")
	}
	guard.Allow(Rule{Scope: "x"})
	if guard.Rules() != nil {
		t.Error("nil 守卫应返回 nil 规则")
	}
	if _, matched := guard.Match("GET", "/any"); matched {
		t.Error("nil 守卫不应命中任何规则")
	}
}

func TestHasScope(t *testing.T) {
	cases := []struct {
		granted  string
		required string
		want     bool
	}{
		{"huobao.read", "huobao.read", true},
		{"huobao.read,huobao.write", "huobao.write", true},
		{" huobao.read , huobao.write ", "HUOBAO.WRITE", true},
		{"huobao.read", "huobao.write", false},
		{"", "huobao.read", false},
		{"huobao.read", "", true},
	}

	for _, tc := range cases {
		if got := hasScope(tc.granted, tc.required); got != tc.want {
			t.Errorf("hasScope(%q, %q) = %v，期望 %v", tc.granted, tc.required, got, tc.want)
		}
	}
}
