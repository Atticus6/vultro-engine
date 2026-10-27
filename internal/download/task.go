// Package download 执行单个离线下载任务：从源站逐片取字节、回传 Worker 的上传会话。
//
// v1 不落盘：每片 ≤64 MiB 缓冲在内存（与 Worker 侧分片上限一致），下载完一片传一片。
// 断点续传不需要引擎侧任何持久化：改派时 Worker 在派发载荷里给出已传分片号
// （来自 R2 ListParts），跳过即可。
package download

// Task 派发载荷（Worker → 引擎，`POST /v1/tasks` 的 body）。
// JSON 字段名与 TS 侧 `DispatchPayload` 逐字对应。
type Task struct {
	TaskID    string `json:"taskId"`
	TaskToken string `json:"taskToken"`
	Source    struct {
		URL string `json:"url"`
	} `json:"source"`
	Session struct {
		ID            string `json:"id"`
		Mode          string `json:"mode"` // single | multipart
		PartSize      int64  `json:"partSize"`
		PartCount     int    `json:"partCount"`
		Size          int64  `json:"size"`
		UploadedParts []int  `json:"uploadedParts"`
	} `json:"session"`
	Egress struct {
		Mode string `json:"mode"`
	} `json:"egress"`
	DeadlineMs int64 `json:"deadlineMs"`
}
