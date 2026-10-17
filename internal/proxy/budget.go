package proxy

import "strings"

// 请求类型。设计文档 §6 的四级预算按请求类型划分，入口不做业务路由登记，
// 因此用「方法 + 路径片段」把请求归到某一档。
const (
	// KindRead 是普通读取（GET/HEAD/OPTIONS）。
	KindRead = "read"
	// KindWrite 是普通写入（POST/PUT/PATCH/DELETE）。
	KindWrite = "write"
	// KindGenerate 是长耗时生成类请求。
	KindGenerate = "generate"
	// KindUpload 是大文件上传类请求。
	KindUpload = "upload"
)

// BudgetRules 是请求分类规则：路径命中片段即归入对应档位。
type BudgetRules struct {
	GeneratePaths []string
	UploadPaths   []string
}

// KindOf 判定请求类型。
//
// 规则（按优先级）：
//  1. 路径命中上传片段 → upload；
//  2. 路径命中生成片段 → generate；
//  3. 其余按方法分：GET/HEAD/OPTIONS 走读预算，写方法走写预算。
//
// 路径使用入口侧原始路径判断，不依赖上游改写结果，保证规则可预测。
func KindOf(method, path string, rules BudgetRules) string {
	normalized := strings.ToLower(path)

	if matchAny(normalized, rules.UploadPaths) {
		return KindUpload
	}
	if matchAny(normalized, rules.GeneratePaths) {
		return KindGenerate
	}

	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS":
		return KindRead
	default:
		return KindWrite
	}
}

func matchAny(path string, fragments []string) bool {
	for _, fragment := range fragments {
		if fragment == "" {
			continue
		}
		if strings.Contains(path, strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}
