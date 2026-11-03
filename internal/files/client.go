package files

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"onessh/internal/sshpool"
)

type cached struct {
	ssh  *ssh.Client
	sftp *sftp.Client
}
type ClientPool struct {
	pool *sshpool.Pool
	mu   sync.Mutex
	// locks 为每个主机提供独立的锁。建立 SFTP 客户端要跨网络完成 SSH 通道握手，
	// 一把全局锁会让一台慢主机把整个网关的文件操作全部串行化。
	locks   map[string]*sync.Mutex
	clients map[string]cached
	closed  bool
}

func NewClientPool(pool *sshpool.Pool) *ClientPool {
	return &ClientPool{pool: pool, clients: make(map[string]cached), locks: make(map[string]*sync.Mutex)}
}

// hostLock 返回该主机专用的锁。锁的获取顺序固定为「先 hostLock 再 mu」，
// 而 Invalidate 与 Close 只取 mu，因此不存在逆序等待导致的死锁。
func (p *ClientPool) hostLock(host string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	lock := p.locks[host]
	if lock == nil {
		lock = &sync.Mutex{}
		p.locks[host] = lock
	}
	return lock
}

func (p *ClientPool) Get(ctx context.Context, host string) (*sftp.Client, error) {
	sshClient, err := p.pool.Get(ctx, host)
	if err != nil {
		return nil, err
	}
	lock := p.hostLock(host)
	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	if c, ok := p.clients[host]; ok && c.ssh == sshClient {
		p.mu.Unlock()
		return c.sftp, nil
	}
	if old, ok := p.clients[host]; ok {
		old.sftp.Close()
		// 连同条目一起摘掉：随后 NewClient 失败时，池里不能留着一个已关闭的旧客户端，
		// 否则下一次 Get 会拿 sshClient 与它比对失败、反复重建。
		delete(p.clients, host)
	}
	p.mu.Unlock()
	// 释放后再做网络握手：这一步可能耗掉整个 SSH 超时，不该让其他主机排队等待。
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		p.pool.Invalidate(host)
		return nil, fmt.Errorf("创建 SFTP 客户端: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		client.Close()
		return nil, fmt.Errorf("文件连接池已关闭")
	}
	p.clients[host] = cached{ssh: sshClient, sftp: client}
	return client, nil
}
func (p *ClientPool) Invalidate(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[host]; ok {
		c.sftp.Close()
		delete(p.clients, host)
	}
}
func (p *ClientPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, c := range p.clients {
		c.sftp.Close()
	}
	p.clients = make(map[string]cached)
}
func resolvePath(c *sftp.Client, path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := c.Getwd()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return home + "/" + strings.TrimPrefix(path, "~/"), nil
}
