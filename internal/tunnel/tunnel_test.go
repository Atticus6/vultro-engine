// 隧道辅助函数的单测：监听地址 → 隧道应访问的本机 URL。
package tunnel

import "testing"

func TestLocalURL(t *testing.T) {
	cases := map[string]string{
		":8787":          "http://127.0.0.1:8787", // 通配监听 → 回环
		"0.0.0.0:8787":   "http://127.0.0.1:8787",
		"[::]:8787":      "http://127.0.0.1:8787",
		"127.0.0.1:8787": "http://127.0.0.1:8787",
		"garbage":        "http://garbage",
	}
	for input, want := range cases {
		if got := LocalURL(input); got != want {
			t.Errorf("LocalURL(%q) = %q, want %q", input, got, want)
		}
	}
}
