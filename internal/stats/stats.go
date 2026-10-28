// Package stats 收集引擎自身的运行指标：**内存占用**与**上下行流量累计**。
//
// 只做记账，不做聚合：心跳（每 30s，见 internal/worker 的 Heartbeat）把「累计值 +
// 进程启动时间」原样上报，按日求和、处理进程重启全部在 Worker 侧完成（见
// `src/server/drive/offline.ts` 的 engineHeartbeat）。引擎因此不需要任何持久化——
// 一个纯粹的下载器不该为了统计数字写盘。
//
// ## 为什么要带启动时间
//
// 累计值在进程重启后从 0 重新开始，而 Worker 只看到"数字变小了"。若把这种变小直接
// 当作 0 增量，重启前丢掉的那一段就永远算不回来；若按差值算，会得到一个负数被吞掉。
// 让引擎自报"这笔账是哪个进程的"（startedAtMs），Worker 就能把差值重算换成"全新
// 一轮的累计值"，重启边界不丢账、也不会重复计。
//
// ## 流量口径
//
// 两个方向分开记，因为它们的意义不同（下行 = 源站取流，上行 = 回传 Worker）：
//
//   - rx（下行）：从源站（relay 模式下是经 Worker 的 fetch 出口）**实际读出的字节**。
//     按读出的字节而不是分片长度记账：失败重试的那一片同样消耗了带宽；
//   - tx（上行）：分片上传请求体**被 HTTP 客户端读出的字节**。它包含未成功那次的
//     部分传输——那也确实用掉了流量。
//
// 两者都由 {@link Counters} 的包装 reader 驱动，因此计数点只有一处，不会出现
// "某条路径忘了加"的统计裂缝。
package stats

import (
	"io"
	"runtime"
	"sync/atomic"
	"time"
)

// Counters 进程级计数器（并发安全，零值不可用——用 {@link New}）。
type Counters struct {
	// startedAtMs 本次进程启动的 Unix 毫秒时间戳。
	//
	// 它是 Worker 侧识别"累计值归零"的唯一依据（见包注释），所以必须在进程最早的时刻
	// 取一次并保持不变：中途重新取会让 Worker 误判为重启，把已有累计值当成新增量。
	startedAtMs int64

	rx atomic.Int64
	tx atomic.Int64
}

// New 创建计数器，并把当前时刻记为进程启动时间。
func New() *Counters {
	return &Counters{startedAtMs: time.Now().UnixMilli()}
}

// Snapshot 一次取齐三个值（心跳载荷的原样来源）。
type Snapshot struct {
	// RxBytes 下行累计字节（自进程启动起算，单调递增）。
	RxBytes int64
	// TxBytes 上行累计字节（同上）。
	TxBytes int64
	// StartedAtMs 进程启动时刻（Unix 毫秒）。Worker 用它判断累计值是否重置。
	StartedAtMs int64
}

// Snapshot 读取当前累计值。
//
// 三个值不是原子地一起取出的（两次 Load 之间可能刚好又读了几个字节）。这不影响
// 正确性：Worker 侧的增量是 max(0, 新值 - 旧值)，多算的那几个字节下一轮自然不再出现。
func (c *Counters) Snapshot() Snapshot {
	return Snapshot{
		RxBytes:     c.rx.Load(),
		TxBytes:     c.tx.Load(),
		StartedAtMs: c.startedAtMs,
	}
}

// RxReader 包一层：读出的字节计入下行。用于源站响应体。
func (c *Counters) RxReader(rc io.ReadCloser) io.ReadCloser {
	return &countingReadCloser{
		countingReader: countingReader{src: rc, add: func(n int64) { c.rx.Add(n) }},
		closer:         rc,
	}
}

// TxReader 包一层：读出的字节计入上行。用于上传请求体（HTTP 客户端读取它来写连接）。
func (c *Counters) TxReader(r io.Reader) io.Reader {
	return &countingReader{src: r, add: func(n int64) { c.tx.Add(n) }}
}

// countingReader 把每次成功读出的字节数回调出去。
//
// 只统计 `n > 0` 的读：`Read` 允许返回 (0, nil)（例如网络暂时无数据），那不是流量。
type countingReader struct {
	src io.Reader
	add func(int64)
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.add(int64(n))
	}
	return n, err
}

// countingReadCloser 保留底层 Close：调用方仍然按 ReadCloser 契约关闭响应体。
type countingReadCloser struct {
	countingReader
	closer io.Closer
}

func (r *countingReadCloser) Close() error {
	return r.closer.Close()
}

// Memory 一次取回的内存指标（字节）。
type Memory struct {
	// HeapBytes Go 堆上**在用**的对象字节数（`MemStats.HeapAlloc`）。
	//
	// 它对应"程序现在真正拿着多少内存"，是页面上显示的主数字。
	HeapBytes uint64
	// SysBytes 从操作系统取得的总字节数（`MemStats.Sys`，含堆预留、栈、运行时结构）。
	//
	// 与容器内存限制（`docker stats` 的 RSS）最接近的口径，但仍是 Go 自己的记账。
	SysBytes uint64
}

// ReadMemory 读取当前内存指标。
//
// 用 `runtime.ReadMemStats` 而不是平台 API（/proc、GetProcessMemoryInfo）：
// 后者每个平台一套实现、还要处理容器 cgroup，而这里只需要一个"这个 Go 程序占了多少
// 内存"的量级数字。代价是 `ReadMemStats` 会短暂停止世界——心跳每 30s 才调一次，可忽略。
func ReadMemory() Memory {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return Memory{HeapBytes: m.HeapAlloc, SysBytes: m.Sys}
}
