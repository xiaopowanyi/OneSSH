package web

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed dist/*
var assets embed.FS

// immutableCache 用于带内容哈希的产物：Vite 产出的 assets 文件名随内容变化，
// 内容不变则 URL 不变，因此可以放心地让浏览器长期缓存。
const immutableCache = "public, max-age=31536000, immutable"

// maxCachedAssetSize 限制缓存的单个资源体积。正常产物最大约 1.9 MB，
// 这里只是防御异常资源把内存吃光。
const maxCachedAssetSize = 8 << 20

// gzipCache 缓存「路径 → gzip 后内容」。二进制内的静态资源是只读的，
// 每次请求重新压缩一次近 2 MB 的主包没有意义。
var gzipCache sync.Map

func Handler() http.Handler {
	dist, _ := fs.Sub(assets, "dist")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.URL.Path != "/" {
			name := strings.TrimPrefix(r.URL.Path, "/")
			if body, err := fs.ReadFile(dist, name); err == nil {
				serveAsset(w, r, name, body)
				return
			}
		}
		// 读不到的路径一律回落到 SPA 入口：/oauth/authorize、/hosts 这类前端路由
		// 在二进制里并没有对应文件，交给文件服务器只会得到 404。
		index, _ := fs.ReadFile(dist, "index.html")
		serveAsset(w, r, "index.html", index)
	})
}

// serveAsset 送出单个静态资源：先按类型决定是否 gzip，再用 ServeContent 处理
// Range 与条件请求。index.html 是 SPA 入口且文件名不带哈希，必须每次回源校验，
// 否则发布新版本后浏览器仍会停留在旧入口。
func serveAsset(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	contentType := assetContentType(name)
	w.Header().Set("Content-Type", contentType)
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", immutableCache)
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if compressed, ok := gzipVariant(r, name, body, contentType); ok {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		body = compressed
	}
	// embed.FS 不提供修改时间，传入零值即不发送 Last-Modified；
	// 缓存控制完全交给上面的 Cache-Control。
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}

// gzipVariant 返回该资源的 gzip 版本。只对文本类并且客户端明确接受 gzip 时启用：
// PNG、JPEG、woff2 等本身已经是压缩格式，再压一遍只会浪费 CPU。
// assetContentType 推导静态资源的 Content-Type。
//
// 前端用到的类型就那么几个，这里刻意写死而不完全依赖 mime.TypeByExtension：
// 后者在 Windows 上会去读注册表 HKEY_CLASSES_ROOT\<ext>\Content Type，很多机器上
// .js 这一项要么缺失，要么被装过的编辑器/脚本宿主改成了 text/plain。浏览器对
// <script type="module"> 是强制执行严格 MIME 检查的，拿到 text/plain 会直接拒绝
// 执行模块脚本，整个页面白屏且不加载任何 JS。宿主注册表不该决定我们能否启动。
func assetContentType(name string) string {
	dot := strings.LastIndex(name, ".")
	// 没有扩展名的文件（例如根目录的 LICENSE）会让 LastIndex 返回 -1，直接切片会越界。
	if dot < 0 {
		return "application/octet-stream"
	}
	switch ext := strings.ToLower(name[dot:]); ext {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".wasm":
		return "application/wasm"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".ico":
		return "image/x-icon"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	default:
		// 兜底仍查一次注册表/系统表，未知类型不至于退化成下载。
		if detected := mime.TypeByExtension(ext); detected != "" {
			return detected
		}
		return "application/octet-stream"
	}
}

func gzipVariant(r *http.Request, name string, body []byte, contentType string) ([]byte, bool) {
	if !compressible(contentType) || len(body) < 1024 || len(body) > maxCachedAssetSize {
		return nil, false
	}
	// Range 与 gzip 不能同时生效：ServeContent 会对「已经压缩过」的字节流做范围切片，
	// 切出来的片段既没有完整的 gzip 头也没有尾部 CRC，客户端解压会直接报流损坏。
	// 静态资源本就不大，这种组合直接退回未压缩的原始字节。
	if r.Header.Get("Range") != "" {
		return nil, false
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
		return nil, false
	}
	if cached, ok := gzipCache.Load(name); ok {
		return cached.([]byte), true
	}
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(body); err != nil {
		return nil, false
	}
	if err := writer.Close(); err != nil {
		return nil, false
	}
	encoded := buf.Bytes()
	gzipCache.Store(name, encoded)
	return encoded, true
}

func compressible(contentType string) bool {
	if strings.HasPrefix(contentType, "text/") || strings.HasPrefix(contentType, "image/svg") {
		return true
	}
	for _, suffix := range []string{"javascript", "json", "xml", "wasm"} {
		if strings.Contains(contentType, suffix) {
			return true
		}
	}
	return false
}
