// Package lb 提供随机 + 权重负载均衡与灰度实例筛选。
//
// 筛选与挑选语义 1:1 复刻 yudao-gateway 的 GrayLoadBalancer：
//   - 请求头 version 非空 → 只保留 metadata.version 相等的实例；
//     若筛完为空，回退为全部实例（并让调用方记录告警）；
//   - 请求头 tag 为空 → 剔除打了 tag 的实例，避免流量意外打到定向实例；
//     若筛完为空，回退为全部实例；
//   - 请求头 tag 非空 → 只保留 metadata.tag 相等的实例；
//     若筛完为空，回退为全部实例；
//   - 最终按权重随机选取一个实例（等价 NacosBalancer#getHostByRandomWeight3）。
package lb

import (
	"math/rand"

	"newx-gateway/internal/discovery"
)

// Pick 随机 + 权重地选取一个可用实例。
func Pick(instances []discovery.Instance) (discovery.Instance, bool) {
	return PickWith(rand.New(rand.NewSource(randSeed())), instances)
}

// PickWith 使用指定随机源选取实例，便于测试复现。
func PickWith(rnd *rand.Rand, instances []discovery.Instance) (discovery.Instance, bool) {
	usable := usableInstances(instances)
	if len(usable) == 0 {
		return discovery.Instance{}, false
	}

	total := 0.0
	for _, instance := range usable {
		if weight := instance.Weight; weight > 0 {
			total += weight
		}
	}

	// 全部实例权重都为 0 时退化为均摊随机，避免除零与永远选不到实例。
	if total <= 0 {
		return usable[rnd.Intn(len(usable))], true
	}

	target := rnd.Float64() * total
	accumulated := 0.0
	for _, instance := range usable {
		if weight := instance.Weight; weight > 0 {
			accumulated += weight
		}
		if target < accumulated {
			return instance, true
		}
	}
	// 浮点累加误差兜底：返回最后一个可用实例。
	return usable[len(usable)-1], true
}

// FilterGray 按 version 与 tag 筛选实例，语义见包注释。
//
// 返回值第二个布尔表示是否发生了「回退到全量」——调用方应记录告警，
// 因为这意味着灰度配置与线上实例不匹配。
func FilterGray(instances []discovery.Instance, version, tag string) ([]discovery.Instance, bool) {
	filtered := usableInstances(instances)
	if len(filtered) == 0 {
		return filtered, false
	}
	// full 是未经任何灰度筛选的全量实例，用于回退。
	full := filtered
	restored := false

	// 第一步：按 version 筛选。无匹配时回退全量。
	if version != "" {
		filtered = filterBy(filtered, func(instance discovery.Instance) bool {
			return instance.Version() == version
		})
		if len(filtered) == 0 {
			filtered = full
			restored = true
		}
	}

	// 第二步：按 tag 筛选。无匹配时同样回退全量。
	switch {
	case tag == "":
		filtered = filterBy(filtered, func(instance discovery.Instance) bool {
			return instance.Tag() == ""
		})
		if len(filtered) == 0 {
			filtered = full
			restored = true
		}
	default:
		filtered = filterBy(filtered, func(instance discovery.Instance) bool {
			return instance.Tag() == tag
		})
		if len(filtered) == 0 {
			filtered = full
			restored = true
		}
	}

	return filtered, restored
}

// usableInstances 过滤出可参与负载均衡的实例，并返回副本。
func usableInstances(instances []discovery.Instance) []discovery.Instance {
	usable := make([]discovery.Instance, 0, len(instances))
	for _, instance := range instances {
		if instance.Usable() {
			usable = append(usable, instance.Clone())
		}
	}
	return usable
}

func filterBy(instances []discovery.Instance, keep func(discovery.Instance) bool) []discovery.Instance {
	filtered := make([]discovery.Instance, 0, len(instances))
	for _, instance := range instances {
		if keep(instance) {
			filtered = append(filtered, instance)
		}
	}
	return filtered
}
