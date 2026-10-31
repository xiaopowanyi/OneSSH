package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLRoutesDenyFraming(t *testing.T) {
	handler := Handler()
	for _, path := range []string{"/", "/login", "/oauth/authorize"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("状态码 = %d", response.Code)
			}
			if got := response.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
				t.Fatalf("Content-Security-Policy = %q", got)
			}
			if got := response.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Fatalf("X-Frame-Options = %q", got)
			}
		})
	}
}

// Windows 的 mime.TypeByExtension 会去读注册表 HKEY_CLASSES_ROOT\<ext>\Content Type，
// 很多机器上 .js 缺失或被编辑器/脚本宿主改成了 text/plain。浏览器对
// <script type="module"> 强制严格 MIME 检查，收到错误类型就拒绝执行模块脚本，
// 表现为整页白屏。因此前端用到的这几个类型必须写死，不受宿主环境左右。
func TestAssetContentTypeIgnoresHostRegistry(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"assets/index-abc123.js", "text/javascript; charset=utf-8"},
		{"assets/index-abc123.mjs", "text/javascript; charset=utf-8"},
		{"assets/index-abc123.css", "text/css; charset=utf-8"},
		{"index.html", "text/html; charset=utf-8"},
		{"logo.svg", "image/svg+xml"},
		{"app.wasm", "application/wasm"},
		{"data.json", "application/json; charset=utf-8"},
		{"LICENSE", "application/octet-stream"},
	} {
		if got := assetContentType(tc.name); got != tc.want {
			t.Errorf("assetContentType(%q) = %q, 期望 %q", tc.name, got, tc.want)
		}
	}
}

// Range 与 gzip 不能叠加：ServeContent 会对已压缩的字节流做范围切片，客户端解压会失败。
func TestRangeRequestSkipsGzip(t *testing.T) {
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(sub, "assets")
	if err != nil {
		t.Skip("构建产物中没有 assets 目录")
	}
	target := ""
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".js") {
			target = "/assets/" + entry.Name()
			break
		}
	}
	if target == "" {
		t.Skip("构建产物中没有 js 文件")
	}
	handler := Handler()

	withGzip := httptest.NewRequest(http.MethodGet, target, nil)
	withGzip.Header.Set("Accept-Encoding", "gzip")
	gzipResponse := httptest.NewRecorder()
	handler.ServeHTTP(gzipResponse, withGzip)
	if got := gzipResponse.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("仅带 Accept-Encoding 时应启用 gzip，实际 = %q", got)
	}

	ranged := httptest.NewRequest(http.MethodGet, target, nil)
	ranged.Header.Set("Accept-Encoding", "gzip")
	ranged.Header.Set("Range", "bytes=0-1023")
	rangeResponse := httptest.NewRecorder()
	handler.ServeHTTP(rangeResponse, ranged)
	if got := rangeResponse.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("带 Range 时不应启用 gzip，实际 Content-Encoding = %q", got)
	}
}
