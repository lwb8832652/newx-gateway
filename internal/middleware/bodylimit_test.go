package middleware

import "testing"

func TestIsUploadPath(t *testing.T) {
	paths := []string{"/upload"}

	cases := map[string]bool{
		"/web/huobao/v1/upload":       true,
		"/web/huobao/v1/upload/image": true,
		"/web/huobao/v1/dramas":       false,
		"/UPLOAD":                     true, // 比较大小写不敏感
		"":                            false,
	}

	for path, expected := range cases {
		if got := IsUploadPath(path, paths); got != expected {
			t.Errorf("IsUploadPath(%q) = %v，期望 %v", path, got, expected)
		}
	}
}

func TestIsUploadPathEmptyRules(t *testing.T) {
	if IsUploadPath("/web/huobao/v1/upload", nil) {
		t.Error("无规则时不应有任何路径命中上传通道")
	}
	if IsUploadPath("/web/huobao/v1/upload", []string{""}) {
		t.Error("空片段应被忽略")
	}
}

func TestLimitForUploadExemption(t *testing.T) {
	cases := []struct {
		name          string
		path          string
		maxBytes      int64
		uploadPaths   []string
		uploadMaxByte int64
		want          int64
	}{
		{"上传路径用上传上限", "/web/huobao/v1/upload", 1 << 20, []string{"/upload"}, 2 << 30, 2 << 30},
		{"普通路径用普通上限", "/web/huobao/v1/dramas", 1 << 20, []string{"/upload"}, 2 << 30, 1 << 20},
		{"上传上限更小时回落", "/web/huobao/v1/upload", 1 << 20, []string{"/upload"}, 1 << 10, 1 << 20},
		{"无上传规则", "/web/huobao/v1/upload", 1 << 20, nil, 2 << 30, 1 << 20},
		{"上限 0 表示不限制", "/any", 0, nil, 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LimitFor(tc.path, tc.maxBytes, tc.uploadPaths, tc.uploadMaxByte)
			if got != tc.want {
				t.Errorf("LimitFor = %d，期望 %d", got, tc.want)
			}
		})
	}
}

func TestIsUploadPathHandlesSubstring(t *testing.T) {
	if !IsUploadPath("/api/v1/batch/upload/artwork", []string{"upload"}) {
		t.Error("片段匹配应支持子串")
	}
}
