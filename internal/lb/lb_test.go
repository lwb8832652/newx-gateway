package lb

import (
	"math/rand"
	"testing"

	"newx-gateway/internal/discovery"
)

func instance(host string, weight float64, version, tag string) discovery.Instance {
	return discovery.Instance{
		Host:     host,
		Port:     8080,
		Scheme:   "http",
		Weight:   weight,
		Healthy:  true,
		Enabled:  true,
		Metadata: map[string]string{"version": version, "tag": tag},
	}
}

func TestPickHonorsWeights(t *testing.T) {
	instances := []discovery.Instance{
		instance("a", 1, "", ""),
		instance("b", 9, "", ""),
	}

	rnd := rand.New(rand.NewSource(42))
	counts := map[string]int{}
	for i := 0; i < 10000; i++ {
		chosen, ok := PickWith(rnd, instances)
		if !ok {
			t.Fatal("应能选出实例")
		}
		counts[chosen.Host]++
	}

	// 权重 1:9 → 期望约 10% : 90%，留出统计余量。
	if counts["b"] < 8000 || counts["b"] > 9800 {
		t.Errorf("权重分布异常: %v（期望 b 约占九成）", counts)
	}
}

func TestPickSkipsUnusableInstances(t *testing.T) {
	dead := instance("dead", 100, "", "")
	dead.Healthy = false
	disabled := instance("disabled", 100, "", "")
	disabled.Enabled = false
	noPort := instance("noport", 100, "", "")
	noPort.Port = 0

	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		chosen, ok := PickWith(rnd, []discovery.Instance{dead, disabled, noPort, instance("alive", 1, "", "")})
		if !ok {
			t.Fatal("可用实例存在时应能选中")
		}
		if chosen.Host != "alive" {
			t.Errorf("选中了不可用实例: %q", chosen.Host)
		}
	}
}

func TestPickWithZeroWeightsIsUniform(t *testing.T) {
	instances := []discovery.Instance{
		instance("a", 0, "", ""),
		instance("b", 0, "", ""),
	}

	rnd := rand.New(rand.NewSource(7))
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		chosen, ok := PickWith(rnd, instances)
		if !ok {
			t.Fatal("权重全零时也应能选中")
		}
		seen[chosen.Host] = true
	}
	if len(seen) != 2 {
		t.Errorf("权重全零应退化为均摊随机，实际只选中 %v", seen)
	}
}

func TestPickEmptyReturnsFalse(t *testing.T) {
	if _, ok := PickWith(rand.New(rand.NewSource(1)), nil); ok {
		t.Error("空实例列表应返回 false")
	}
	if _, ok := PickWith(rand.New(rand.NewSource(1)), []discovery.Instance{}); ok {
		t.Error("空实例列表应返回 false")
	}
}

func TestFilterGrayByVersion(t *testing.T) {
	instances := []discovery.Instance{
		instance("v1-a", 1, "1.0", ""),
		instance("v1-b", 1, "1.0", ""),
		instance("v2-a", 1, "2.0", ""),
	}

	filtered, fellBack := FilterGray(instances, "1.0", "")
	if fellBack {
		t.Error("命中的灰度筛选不应标记回退")
	}
	if len(filtered) != 2 {
		t.Fatalf("筛选结果 = %d，期望 2", len(filtered))
	}
	for _, item := range filtered {
		if item.Version() != "1.0" {
			t.Errorf("混入了非目标版本实例: %q", item.Host)
		}
	}

	// version 不匹配任何实例 → 回退全量。
	filtered, fellBack = FilterGray(instances, "9.9", "")
	if !fellBack {
		t.Error("version 无匹配时应标记回退")
	}
	if len(filtered) != 3 {
		t.Errorf("回退后应为全量，实际 %d", len(filtered))
	}
}

func TestFilterGrayByTag(t *testing.T) {
	instances := []discovery.Instance{
		instance("plain", 1, "", ""),
		instance("dev-machine", 1, "", "dev"),
	}

	// 请求无 tag：剔除打了 tag 的实例，避免流量误打定向机器。
	filtered, fellBack := FilterGray(instances, "", "")
	if fellBack || len(filtered) != 1 || filtered[0].Host != "plain" {
		t.Errorf("无 tag 时应只保留未打标实例: %+v fellBack=%v", filtered, fellBack)
	}

	// 请求带 tag：只保留匹配的实例。
	filtered, fellBack = FilterGray(instances, "", "dev")
	if fellBack || len(filtered) != 1 || filtered[0].Host != "dev-machine" {
		t.Errorf("带 tag 时应只保留匹配实例: %+v fellBack=%v", filtered, fellBack)
	}

	// tag 无匹配：回退全量。
	filtered, fellBack = FilterGray(instances, "", "prod")
	if !fellBack || len(filtered) != 2 {
		t.Errorf("tag 无匹配时应回退全量: %+v fellBack=%v", filtered, fellBack)
	}
}

func TestFilterGrayVersionThenTag(t *testing.T) {
	instances := []discovery.Instance{
		instance("v1-plain", 1, "1.0", ""),
		instance("v1-dev", 1, "1.0", "dev"),
		instance("v2-dev", 1, "2.0", "dev"),
	}

	// 先按 version 筛，再按 tag 筛，两步顺序不可交换。
	filtered, fellBack := FilterGray(instances, "1.0", "")
	if fellBack || len(filtered) != 1 || filtered[0].Host != "v1-plain" {
		t.Errorf("version+tag 组合筛选异常: %+v fellBack=%v", filtered, fellBack)
	}
}

func TestFilterGrayAllUnusable(t *testing.T) {
	dead := instance("dead", 1, "", "")
	dead.Healthy = false

	filtered, fellBack := FilterGray([]discovery.Instance{dead}, "1.0", "")
	if fellBack || len(filtered) != 0 {
		t.Errorf("全部不可用时应返回空且不标记回退: %+v fellBack=%v", filtered, fellBack)
	}
}
