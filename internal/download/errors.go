// 分片长度、错误分类与可重试判定——都是与 Worker 侧契约对齐的纯函数，便于单测。
package download

import (
	"context"
	"errors"
	"strings"

	"github.com/atticus6/vultro-engine/internal/worker"
)

// partLength 第 n 片长度（除末片外等大；与 Worker 侧 expectedPartLength 同一公式）。
func partLength(size, partSize int64, partCount, partNumber int) int64 {
	if partNumber == partCount {
		return size - partSize*int64(partCount-1)
	}
	return partSize
}

// classifyError 引擎侧错误 → 结构化失败码（与 Worker 侧错误码表对齐）。
func classifyError(err error) (code, message string) {
	var permanent *worker.PermanentError
	if errors.As(err, &permanent) {
		if containsAny(permanent.Msg, "404", "410") {
			return "source-error-4xx", permanent.Msg
		}
		return "source-error-5xx", permanent.Msg
	}
	if errors.Is(err, context.DeadlineExceeded) || containsAny(err.Error(), "超时") {
		return "deadline", "任务超时"
	}
	if errors.Is(err, context.Canceled) || containsAny(err.Error(), "取消") {
		return "cancelled", "任务被取消"
	}
	return "source-error-5xx", err.Error()
}

// isPermanent 判断是否不可重试（4xx 语义）。
func isPermanent(err error) bool {
	var permanent *worker.PermanentError
	return errors.As(err, &permanent)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
