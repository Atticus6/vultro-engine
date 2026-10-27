// 命令行参数解析的单测：两种写法、缺值/未知参数的报错、以及 --print-config 的令牌打码。
package main

import (
	"strings"
	"testing"
)

func TestParseArgsInlineAndSeparate(t *testing.T) {
	args, err := parseArgs([]string{
		"--engine-id=off1",
		"--token", "eng_secret",
		"--capacity=4",
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if args.options.ID != "off1" {
		t.Errorf("--engine-id=value 未解析，得到 %q", args.options.ID)
	}
	if args.options.Token != "eng_secret" {
		t.Errorf("--token value 未解析，得到 %q", args.options.Token)
	}
	if args.options.Capacity != "4" {
		t.Errorf("--capacity 未解析，得到 %q", args.options.Capacity)
	}
}

func TestParseArgsRejectsUnknownAndMissingValue(t *testing.T) {
	if _, err := parseArgs([]string{"--nope"}); err == nil {
		t.Fatal("未知参数应报错（不能静默忽略）")
	}
	if _, err := parseArgs([]string{"--token"}); err == nil {
		t.Fatal("缺值应报错（不能把下一个参数当成值）")
	}
}

func TestParseArgsHelpAndPrintConfig(t *testing.T) {
	args, err := parseArgs([]string{"--print-config", "--help"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// `--help` 优先：看到就立刻返回，不再解析后续参数。
	if !args.showHelp {
		t.Error("--help 应被识别")
	}
}

func TestMaskToken(t *testing.T) {
	cases := map[string]string{
		"":                                   "（未设置）",
		"short":                              "***",
		"eng_abcdefghijklmnopqrstuvwxyz0123": "eng_abcd…0123",
	}
	for input, want := range cases {
		if got := maskToken(input); got != want {
			t.Errorf("maskToken(%q) = %q, want %q", input, got, want)
		}
	}
	// 打码后的值绝不能包含完整令牌。
	full := "eng_abcdefghijklmnopqrstuvwxyz0123"
	if strings.Contains(maskToken(full), full) {
		t.Error("打码结果不应包含完整令牌")
	}
}
