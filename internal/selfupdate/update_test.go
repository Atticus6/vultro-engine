// 自更新的可测部分：资产命名、校验和解析、归档解包、最新版本解析。
//
// 不做"真的替换当前二进制"的测试——那会在这个进程上执行 exec，而且需要真实的
// release 资产；替换逻辑的正确性靠冒烟检查（新二进制 --version）与原子 rename 保证。
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleaseAssetName(t *testing.T) {
	// 与 .goreleaser.yml 的 name_template 一致：版本不带 v 前缀。
	got := releaseAssetName("v1.2.3", "linux", "arm64")
	want := "vultro-engine_1.2.3_linux_arm64.tar.gz"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// makeArchive 造一个只含单个文件的 tar.gz。
func makeArchive(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	payload := []byte("#!/bin/sh\necho hi\n")
	archive := makeArchive(t, "vultro-engine", payload)

	got, err := extractBinary(archive, "vultro-engine")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("内容不符: %q", got)
	}

	if _, err := extractBinary(archive, "other-bin"); err == nil {
		t.Fatal("缺失文件时应该报错")
	}
}

func TestFetchChecksum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(
			"deadbeef  vultro-engine_1.0.0_linux_amd64.tar.gz\n" +
				"C0FFEE00  vultro-engine_1.0.0_linux_arm64.tar.gz\n",
		))
	}))
	defer server.Close()

	sum, err := fetchChecksum(context.Background(), server.Client(), server.URL, "vultro-engine_1.0.0_linux_arm64.tar.gz")
	if err != nil {
		t.Fatalf("fetchChecksum: %v", err)
	}
	// 统一小写（checksums.txt 里可能是大写）。
	if sum != "c0ffee00" {
		t.Fatalf("got %q", sum)
	}

	if _, err := fetchChecksum(context.Background(), server.Client(), server.URL, "missing.tgz"); err == nil {
		t.Fatal("找不到资产时应报错")
	}
}

func TestLatestReleaseTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 与 GitHub 一样：/releases/latest 302 到 /releases/tag/<tag>。
		w.Header().Set("Location", "/atticus6/vultro-engine/releases/tag/v9.9.9")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	original := githubBase
	githubBase = server.URL
	defer func() { githubBase = original }()

	tag, err := latestReleaseTag(context.Background())
	if err != nil {
		t.Fatalf("latestReleaseTag: %v", err)
	}
	if tag != "v9.9.9" {
		t.Fatalf("got %q", tag)
	}
}

func TestDownloadRejectsNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := download(context.Background(), server.Client(), server.URL); err == nil {
		t.Fatal("404 应报错，而不是返回空内容")
	}
	if !strings.Contains(releaseAssetName("v0.1.0", "darwin", "amd64"), "0.1.0_darwin_amd64") {
		t.Fatal("资产名拼装异常")
	}
}
