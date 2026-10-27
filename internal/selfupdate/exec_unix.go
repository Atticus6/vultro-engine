//go:build unix

package selfupdate

import (
	"os"
	"syscall"
)

// execSelf 用新二进制**原地替换**当前进程（保留 PID、参数与环境变量）。
//
// 为什么用 Exec 而不是"起子进程再退出"：
// - Exec 不会出现"父子进程同时持有一条隧道/同端口"的窗口；
// - 隧道、HTTP 服务、WireGuard 设备全部随进程镜像一起换新，无需手工清理。
func execSelf(path string) error {
	return syscall.Exec(path, os.Args, os.Environ())
}
