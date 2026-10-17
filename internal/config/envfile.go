package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile 读取一个 KEY=VALUE 形式的 env 文件并注入进程环境变量。
//
// 语义（刻意保守）：
//   - 逐行解析，忽略空行、以 # 开头的注释行。
//   - 允许 export 前缀与值两侧引号。
//   - 已存在的进程环境变量优先，文件不覆盖它——保证命令行/容器注入的配置永远最高优先级。
//
// 返回实际注入的条目数与因已存在而跳过的条目数。
func LoadEnvFile(path string) (applied int, skipped int, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("打开 env 文件失败: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// env 文件可能包含较长的密钥行，放宽单行上限。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for lineNo := 1; scanner.Scan(); lineNo++ {
		key, value, ok, parseErr := parseEnvLine(scanner.Text())
		if parseErr != nil {
			return applied, skipped, fmt.Errorf("%s:%d %w", path, lineNo, parseErr)
		}
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			skipped++
			continue
		}
		if setErr := os.Setenv(key, value); setErr != nil {
			return applied, skipped, fmt.Errorf("%s:%d 设置 %s 失败: %w", path, lineNo, key, setErr)
		}
		applied++
	}
	if err := scanner.Err(); err != nil {
		return applied, skipped, fmt.Errorf("读取 env 文件失败: %w", err)
	}
	return applied, skipped, nil
}

// parseEnvLine 解析单行。ok=false 表示该行应被忽略（空行或注释）。
func parseEnvLine(raw string) (key, value string, ok bool, err error) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

	name, val, found := strings.Cut(line, "=")
	if !found {
		return "", "", false, fmt.Errorf("缺少 = 的行无法解析: %q", raw)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false, fmt.Errorf("键名为空: %q", raw)
	}
	val = strings.TrimSpace(val)
	// 去掉成对引号，保留内部内容原样（含空格与 #）。
	if len(val) >= 2 {
		if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
	}
	return name, val, true, nil
}
