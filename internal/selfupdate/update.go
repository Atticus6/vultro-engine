// Package selfupdate 让引擎从 GitHub Release 下载新二进制、校验、原子替换并重启自己。
//
// ## 为什么由引擎自己更新（而不是 Worker 下发文件）
//
// Worker 与引擎之间只有一条 HTTP 隧道、没有文件通道；而 release 资产对双方都是公开的。
// 让引擎自己去下载，Worker 只发"更新到哪个版本"这一条指令，职责最干净。
//
// ## 为什么可以替换正在运行的文件
//
// Linux/macOS 允许 rename 覆盖正在执行的二进制（旧 inode 继续运行到进程结束）。
// 所以流程是：下载 → 校验 → 写到同目录临时文件 → `os.Rename` 覆盖 → `syscall.Exec`
// 用新二进制**原地替换进程**。不需要依赖 systemd/容器的重启策略。
//
// 如果 Exec 失败（少见），进程会以非零码退出，交给守护进程重启——此时新二进制已经就位。
//
// ## 安全与可恢复性
//
//   - 校验和解压失败**绝不覆盖**现有二进制（先写临时文件，全部成功才 rename）；
//   - 替换前用新二进制执行一次 `--version` 做冒烟检查，防止把一个损坏/架构不符的文件
//     装上去把自己弄死；
//   - 需要二进制所在目录可写（`/usr/local/bin` 通常要 root；容器内默认 root 或改用
//     compose 里的镜像更新）。
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/atticus6/vultro-engine/internal/buildinfo"
)

// releaseRepo 发布仓库（与 install.sh / CI 一致）。
const releaseRepo = "atticus6/vultro-engine"

// githubBase 发布站基址。抽成变量是为了单测能指向 httptest 服务器。
var githubBase = "https://github.com"

// updateHTTPTimeout 下载二进制的上限（几十 MB，慢网络也够）。
const updateHTTPTimeout = 5 * time.Minute

// versionRe 允许的版本号形态：`v1.2.3` / `1.2.3`，可带 `-prerelease` 与 `+build` 后缀。
//
// 这是**安全边界**，不是格式洁癖：`SelfUpdate` 的 targetVersion 来自
// `POST /v1/update` 的请求体，会被直接拼进 release 下载 URL。含 `/`、`..`、`?` 的值能让
// URL 指到别的仓库/路径（`checksums.txt` 也从同一处取，校验和拦不住同源攻击），最终
// 把攻击者准备的二进制装到本机并 `syscall.Exec`——一枚泄露的引擎令牌就成了该主机的
// 远程代码执行。所以拼 URL 之前必须先过白名单正则。
var versionRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?(?:\+[0-9A-Za-z][0-9A-Za-z.-]*)?$`)

// normalizeVersion 校验版本号并补 `v` 前缀（goreleaser 的 tag 带 v，资产名不带）。
func normalizeVersion(raw string) (string, error) {
	if !versionRe.MatchString(raw) {
		return "", fmt.Errorf("版本号格式非法：%q", raw)
	}
	if !strings.HasPrefix(raw, "v") {
		return "v" + raw, nil
	}
	return raw, nil
}

// SelfUpdate 把当前进程更新到 targetVersion（空 = 最新 release）。
//
// 成功时**不会返回**：它用 `syscall.Exec` 把进程替换成新二进制（隧道会重建、地址会换，
// 启动流程会把新地址重新注册给 Worker）。失败返回错误，当前二进制保持不变。
func SelfUpdate(ctx context.Context, targetVersion string, log *slog.Logger) error {
	version := targetVersion
	if version == "" {
		latest, err := latestReleaseTag(ctx)
		if err != nil {
			return fmt.Errorf("查询最新版本失败：%w", err)
		}
		version = latest
	}
	// 显式指定的版本与 `releases/latest` 重定向里解析出的版本都要过同一道校验：
	// 后者同样是远端可控的输入（见 versionRe 的说明）。
	version, err := normalizeVersion(version)
	if err != nil {
		return err
	}

	current, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位当前二进制失败：%w", err)
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return fmt.Errorf("解析当前二进制路径失败：%w", err)
	}

	asset := releaseAssetName(version, runtime.GOOS, runtime.GOARCH)
	base := fmt.Sprintf("%s/%s/releases/download/%s", githubBase, releaseRepo, version)
	log.Info("开始自更新", "from", buildinfo.Version, "to", version, "asset", asset)

	client := &http.Client{Timeout: updateHTTPTimeout}

	// 1) 校验和
	expected, err := fetchChecksum(ctx, client, base+"/checksums.txt", asset)
	if err != nil {
		return err
	}

	// 2) 下载归档并校验
	archive, err := download(ctx, client, base+"/"+asset)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != expected {
		return fmt.Errorf("校验和不符（下载损坏或资产被替换），已中止")
	}

	// 3) 解出二进制
	binary, err := extractBinary(archive, "vultro-engine")
	if err != nil {
		return err
	}

	// 4) 写到**同目录**临时文件（跨文件系统 rename 会失败），冒烟检查后再覆盖。
	dir := filepath.Dir(current)
	tmp, err := os.CreateTemp(dir, ".vultro-engine.new-*")
	if err != nil {
		return fmt.Errorf("目标目录不可写（自更新需要写 %s 的权限）：%w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("写入新二进制失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		cleanup()
		return fmt.Errorf("设置可执行权限失败：%w", err)
	}

	// 冒烟检查：新二进制至少能打印版本，否则不换。
	smokeCtx, smokeCancel := context.WithTimeout(ctx, 10*time.Second)
	smoke := exec.CommandContext(smokeCtx, tmpPath, "--version")
	out, smokeErr := smoke.CombinedOutput()
	smokeCancel()
	if smokeErr != nil {
		cleanup()
		return fmt.Errorf("新二进制无法运行，已中止：%v（输出：%s）", smokeErr, strings.TrimSpace(string(out)))
	}
	log.Info("新二进制校验通过", "reported", strings.TrimSpace(string(out)))

	// 5) 原子替换 + 原地重生。
	if err := os.Rename(tmpPath, current); err != nil {
		cleanup()
		return fmt.Errorf("替换二进制失败：%w", err)
	}
	log.Info("已替换二进制，重启进程", "path", current)

	if err := execSelf(current); err != nil {
		// rename 已经生效：让进程退出，由守护进程拉起新版本。
		return fmt.Errorf("重启失败（新版本已就位，请重启服务）：%w", err)
	}
	return nil
}

// releaseAssetName 与 `.goreleaser.yml` 的 name_template 必须逐字一致。
func releaseAssetName(version, goos, goarch string) string {
	// goreleaser 的 .Version 不带 v 前缀。
	plain := strings.TrimPrefix(version, "v")
	return fmt.Sprintf("vultro-engine_%s_%s_%s.tar.gz", plain, goos, goarch)
}

// latestReleaseTag 用 releases/latest 的重定向拿到最新 tag（不消耗 API 限流）。
func latestReleaseTag(ctx context.Context) (string, error) {
	client := &http.Client{
		Timeout: 15 * time.Second,
		// 不跟随重定向：Location 就是答案。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/%s/releases/latest", githubBase, releaseRepo), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	idx := strings.LastIndex(location, "/tag/")
	if idx < 0 {
		return "", fmt.Errorf("无法从 %q 解析版本", location)
	}
	return location[idx+len("/tag/"):], nil
}

// fetchChecksum 从 checksums.txt 里取目标资产的 sha256。
func fetchChecksum(ctx context.Context, client *http.Client, url, asset string) (string, error) {
	body, err := download(ctx, client, url)
	if err != nil {
		return "", fmt.Errorf("下载校验和失败：%w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt 里没有 %s", asset)
}

// download 取一个 URL 的完整内容（有大小上限，防止误下超大文件）。
func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 200 MiB 上限：引擎二进制当前 ~46 MiB，留足余量又不至于被恶意响应打爆内存。
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// extractBinary 从 tar.gz 里取出指定名字的文件。
func extractBinary(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, fmt.Errorf("解压失败：%w", err)
	}
	defer gz.Close()

	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("归档里没有 %s", name)
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档失败：%w", err)
		}
		// 归档里的条目形如 `vultro-engine`（goreleaser 不带目录前缀）。
		if filepath.Base(header.Name) != name || header.Typeflag != tar.TypeReg {
			continue
		}
		return io.ReadAll(io.LimitReader(reader, 200<<20))
	}
}
