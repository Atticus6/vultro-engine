//go:build !unix

package main

import "fmt"

// execSelf 非 unix 平台不支持原地替换（本项目只发布 linux/darwin）。
func execSelf(string) error {
	return fmt.Errorf("当前平台不支持自更新重启")
}
