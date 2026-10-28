// 计数器的单测：读出的字节必须被计入对应方向，Close 必须传到下层。
package stats

import (
	"io"
	"runtime"
	"strings"
	"testing"
)

// fakeCloser 记录 Close 是否被调用（下行 reader 必须保留关闭语义）。
type fakeCloser struct {
	io.Reader
	closed bool
}

func (f *fakeCloser) Close() error {
	f.closed = true
	return nil
}

func TestCountersCountReadBytes(t *testing.T) {
	c := New()

	if _, err := io.ReadAll(c.RxReader(io.NopCloser(strings.NewReader("hello")))); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(c.TxReader(strings.NewReader("world!"))); err != nil {
		t.Fatal(err)
	}

	snap := c.Snapshot()
	if snap.RxBytes != 5 {
		t.Errorf("下行应为 5，得到 %d", snap.RxBytes)
	}
	if snap.TxBytes != 6 {
		t.Errorf("上行应为 6，得到 %d", snap.TxBytes)
	}
	if snap.StartedAtMs <= 0 {
		t.Errorf("启动时间应为正数，得到 %d", snap.StartedAtMs)
	}
}

// 只读一部分也必须按实际读出的字节计（不能按声明长度）。
func TestCountersCountPartialReads(t *testing.T) {
	c := New()
	body := c.RxReader(io.NopCloser(strings.NewReader("0123456789")))

	buf := make([]byte, 3)
	if _, err := body.Read(buf); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot().RxBytes; got != 3 {
		t.Errorf("读 3 字节后应为 3，得到 %d", got)
	}
}

// 关闭必须传到下层：源站响应体不关会漏连接。
func TestRxReaderClosesUnderlying(t *testing.T) {
	c := New()
	src := &fakeCloser{Reader: strings.NewReader("x")}
	body := c.RxReader(src)
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if !src.closed {
		t.Error("Close 应传到下层 reader")
	}
}

// 内存指标必须是可用的非零值（真读过运行时，而不是常量）。
func TestReadMemory(t *testing.T) {
	// 先分配一点，保证堆上有东西。
	_ = make([]byte, 1<<20)
	m := ReadMemory()
	if m.HeapBytes == 0 {
		t.Error("HeapBytes 不应为 0")
	}
	if m.SysBytes == 0 {
		t.Error("SysBytes 不应为 0")
	}
	if m.SysBytes < m.HeapBytes {
		t.Errorf("Sys 应不小于 Heap：sys=%d heap=%d", m.SysBytes, m.HeapBytes)
	}
	_ = runtime.MemStats{}
}
