package browser

import "os"

// ChromiumDist 汇集定位 Chromium 的各来源。
type ChromiumDist struct {
	ConfigBinPath string // config 显式指定（最高优先）
	BundledPath   string // 随 App 内置捆绑的固定版（存在才用）
	SystemPath    string // PAL 探测到的系统 Chrome/Edge
}

// resolveChromiumBin 按优先级返回可执行文件路径；返回 "" 表示交给 go-rod launcher 自动下载。
// 优先级：config override > 内置捆绑(存在) > 系统探测 > 空(下载)。
func resolveChromiumBin(d ChromiumDist) string {
	if d.ConfigBinPath != "" {
		return d.ConfigBinPath // 尊重显式配置，不校验存在性（让 launch 阶段报清晰错误）
	}
	if d.BundledPath != "" && binExists(d.BundledPath) {
		return d.BundledPath
	}
	if d.SystemPath != "" {
		return d.SystemPath
	}
	return ""
}

func binExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
