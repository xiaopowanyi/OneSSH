package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"onessh/internal/config"
	"onessh/internal/cryptox"
	"onessh/internal/events"
	"onessh/internal/hostmanager"
	"onessh/internal/mcpserver"
	"onessh/internal/memoryx"
	"onessh/internal/oauthserver"
	"onessh/internal/sshpool"
	"onessh/internal/store"
	"onessh/internal/webapi"
	"onessh/web"
)

var version = "dev"

// gracefulShutdownTimeout 是留给在途 HTTP 请求（含 SSE 与 WebSocket 终端）收尾的时间。
// 这些长连接会一直占用活动连接计数，Shutdown 只能等到它们结束或超时。
const gracefulShutdownTimeout = 15 * time.Second

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	memEngine := memoryx.New(st, memoryx.EmbeddingConfig{
		APIURL: cfg.EmbeddingAPIURL,
		APIKey: cfg.EmbeddingAPIKey,
		Model:  cfg.EmbeddingModel,
	})
	box, err := cryptox.New(cfg.MasterKey)
	if err != nil {
		log.Fatal(err)
	}
	pool := sshpool.New(st, box)
	defer pool.Close()
	hosts := hostmanager.New(st, box, pool)
	bus := events.New()
	// oauthService 先建：它会校验并规范化 ONESSH_PUBLIC_URL，MCP 的 serverInfo 图标直接复用该结果
	oauthService, err := oauthserver.New(st, cfg.PublicURL)
	if err != nil {
		log.Fatal(err)
	}
	mcpService := mcpserver.New(st, pool, bus, hosts, memEngine, mcpserver.Options{
		DataDir:       cfg.DataDir,
		PublicURL:     oauthService.PublicURL,
		PollInterval:  cfg.PollInterval,
		SearchHelper:  cfg.SearchHelper,
		MCPApps:       cfg.MCPApps,
		DisabledTools: cfg.DisabledTools,
	})
	defer mcpService.Close()
	adminAuth := webapi.NewAdminAuth(cfg.AdminPassword, cfg.MasterKey)
	adminAPI := webapi.NewAPI(st, box, pool, hosts, mcpService.Exec, mcpService.Files, mcpService.Jobs, mcpService.Monitor, memEngine, bus)

	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/login", http.HandlerFunc(adminAuth.Login))
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", oauthService.ProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", oauthService.ProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", oauthService.AuthorizationServerMetadata)
	mux.HandleFunc("POST /oauth/register", oauthService.RegisterClient)
	mux.HandleFunc("POST /oauth/token", oauthService.Token)
	mux.Handle("GET /api/v1/oauth/authorization", adminAuth.Require(http.HandlerFunc(oauthService.AuthorizationInfo)))
	mux.Handle("POST /api/v1/oauth/authorization", adminAuth.Require(http.HandlerFunc(oauthService.AuthorizationDecision)))
	mux.Handle("/mcp", mcpserver.Handler(st, mcpService, oauthService.ResourceURLs))
	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", adminAuth.Require(adminAPI.Handler())))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Health(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": version})
	})
	mux.Handle("/", web.Handler())
	// 不设 WriteTimeout：它会掐断 SSE 活动流与 WebSocket 终端这样的长连接。
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       180 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// 没有信号处理的 ListenAndServe 一旦收到 SIGTERM 就直接终止进程，导致本函数所有
	// defer 都不执行：SQLite 来不及 checkpoint，连接池里的 SSH 连接被粗暴断开。
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownCtx.Done()
		log.Printf("收到停止信号，开始优雅停机")
		ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			// 多为仍有 SSH 终端或 SSE 未退出，硬性关掉监听以免进程悬挂。
			log.Printf("优雅停机超时，强制关闭监听: %v", err)
			_ = server.Close()
		}
	}()

	log.Printf("OneSSH %s 监听 %s", version, cfg.Listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.New(os.Stderr, "", 0).Fatal(err)
	}
	// 走到这里监听已经停止，defer 会把 mcpService（后台轮询与文件连接）、连接池和 SQLite
	// 按注册的逆序收掉。
	log.Printf("OneSSH 已停止")
}
