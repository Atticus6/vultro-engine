// Package buildinfo 提供构建期注入的版本号。
//
// 版本号是唯一一个只能由链接器写入的运行时值：GoReleaser、Dockerfile 与本地构建都用
// `-ldflags "-X github.com/atticus6/vultro-engine/internal/buildinfo.Version=<tag>"` 覆盖它。
// 它单独成包，是因为注册、心跳、健康检查、内嵌隧道与自更新日志都要读它——放进任何一个
// 业务包都会让其它包反过来依赖那个业务包。
package buildinfo

// Version 引擎版本（`dev` 表示未经 ldflags 注入的本地构建）。
//
// 会随注册/心跳上报给 Worker，滚动升级时用来判断哪台节点还在跑旧版。
var Version = "dev"
