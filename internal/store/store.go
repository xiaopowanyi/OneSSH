package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/0001_init.sql
var migration0001 string

//go:embed migrations/0002_token_manage_hosts.sql
var migration0002 string

//go:embed migrations/0003_cleanup_orphan_host_refs.sql
var migration0003 string

//go:embed migrations/0004_oauth.sql
var migration0004 string

//go:embed migrations/0005_oauth_code_tombstone.sql
var migration0005 string

//go:embed migrations/0006_memories.sql
var migration0006 string

//go:embed migrations/0007_jump_host.sql
var migration0007 string

//go:embed migrations/0008_audit_token_name.sql
var migration0008 string

//go:embed migrations/0009_audit_tool_index.sql
var migration0009 string

//go:embed migrations/0010_host_tags.sql
var migration0010 string

//go:embed migrations/0011_command_runs.sql
var migration0011 string

//go:embed migrations/0012_audit_command_runs.sql
var migration0012 string

//go:embed migrations/0013_token_disabled_tools.sql
var migration0013 string

//go:embed migrations/0014_job_last_checked.sql
var migration0014 string

//go:embed migrations/0015_host_archive.sql
var migration0015 string

//go:embed migrations/0016_job_indexes.sql
var migration0016 string

//go:embed migrations/0017_performance_indexes.sql
var migration0017 string

//go:embed migrations/0018_metrics_timestamp_index.sql
var migration0018 string

//go:embed migrations/0019_metric_disk_compression.sql
var migration0019 string

// disabledToolsColumns 是迁移 0013 的目标列，与 migrations/0013_token_disabled_tools.sql 一致。
//
// 单独在 Go 里再声明一份，是为了让"迁移 13"和"启动自检"共用同一条 DDL：
// 迁移只按 schema_migrations 里的版本号跳过，历史上一旦出现"版本已记录但 DDL 没生效"
// （脚本为空、WAL 丢失、旧库文件回滚等），这张表就会永久缺列，且再也不会被迁移补回来，
// 运行时所有引用该列的查询都会以 "no such column" 失败。
var disabledToolsColumns = []struct {
	table string
	ddl   string
}{
	{"tokens", `ALTER TABLE tokens ADD COLUMN disabled_tools_json TEXT NOT NULL DEFAULT '[]'`},
	{"oauth_authorization_codes", `ALTER TABLE oauth_authorization_codes ADD COLUMN disabled_tools_json TEXT NOT NULL DEFAULT '[]'`},
	{"oauth_refresh_tokens", `ALTER TABLE oauth_refresh_tokens ADD COLUMN disabled_tools_json TEXT NOT NULL DEFAULT '[]'`},
}

// requiredColumns 是运行时代码强依赖、且允许在启动时补齐的列。
// 只做加法：缺失才 ALTER，绝不删除或改类型。
var requiredColumns = disabledToolsColumns

type Store struct {
	DB      *sql.DB
	writeMu sync.Mutex
}

func sqliteDSN(dbPath string) (string, error) {
	absolutePath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", err
	}
	uriPath := filepath.ToSlash(absolutePath)
	if uriPath[0] != '/' {
		uriPath = "/" + uriPath
	}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	// 写事务在读取前取得写锁，避免 WAL 读快照升级时触发 SQLITE_BUSY_SNAPSHOT。
	query.Add("_txlock", "immediate")
	query.Add("_pragma", "foreign_keys(ON)")
	return (&url.URL{Scheme: "file", Path: uriPath, RawQuery: query.Encode()}).String(), nil
}

func Open(dataDir string) (*Store, error) {
	for _, name := range []string{"artifacts", "command-runs"} {
		dir := filepath.Join(dataDir, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s: %w", name, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("设置数据目录权限 %s: %w", name, err)
		}
	}
	dsn, err := sqliteDSN(filepath.Join(dataDir, "onessh.db"))
	if err != nil {
		return nil, fmt.Errorf("解析 sqlite 路径: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite: %w", err)
	}
	// journal_mode 在不支持 WAL 的文件系统（部分网络挂载、共享目录）上会静默退回 delete：
	// Exec 不报错，但 CheckpointWAL 从此一直失败，写并发也退化成库级锁。这里读回实际值。
	var journalMode string
	if err = db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置数据库参数: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		log.Printf("警告: 数据库所在文件系统不支持 WAL，实际日志模式为 %q，写入将串行化", journalMode)
	}
	if err = migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("执行数据库迁移: %w", err)
	}
	if err = ensureRequiredColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("校验数据库结构: %w", err)
	}
	return &Store{DB: db}, nil
}

// ensureRequiredColumns 在每次启动时补齐缺失的关键列。
//
// 迁移按版本号只跑一次，无法覆盖"版本已记录但 DDL 未生效"的历史库；
// 这里按实际表结构对账，代价是几条 pragma_table_info，换来的是这类库能自愈而不是直接不可用。
func ensureRequiredColumns(db *sql.DB) error {
	for _, col := range requiredColumns {
		exists, err := tableExists(db, col.table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		hasColumn, err := tableHasColumnDB(db, col.table, "disabled_tools_json")
		if err != nil {
			return err
		}
		if hasColumn {
			continue
		}
		log.Printf("数据库结构自检: 表 %s 缺少列 disabled_tools_json，正在补建", col.table)
		if _, err = db.Exec(col.ddl); err != nil {
			// 多实例共享同一个 /data 时，可能同时判定缺列；后一个的 ALTER 会撞上
			// 已存在的列。判列与 ALTER 之间无法原子化，把这种竞争当成功处理。
			if isDuplicateColumnError(err) {
				continue
			}
			return fmt.Errorf("补建 %s.disabled_tools_json: %w", col.table, err)
		}
	}
	return nil
}

// CheckpointWAL performs a WAL checkpoint with TRUNCATE mode.
// Returns (busy, error): busy=true means another connection blocked the checkpoint.
func (s *Store) CheckpointWAL(ctx context.Context) (busy bool, err error) {
	var log, checkpointed int
	err = s.DB.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed)
	return
}

type migration struct {
	version int
	sql     string
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	for _, m := range []migration{
		{version: 1, sql: migration0001},
		{version: 2, sql: migration0002},
		{version: 3, sql: migration0003},
		{version: 4, sql: migration0004},
		{version: 5, sql: migration0005},
		{version: 6, sql: migration0006},
		{version: 7, sql: migration0007},
		{version: 8, sql: migration0008},
		{version: 9, sql: migration0009},
		{version: 10, sql: migration0010},
		{version: 11, sql: migration0011},
		{version: 12, sql: migration0012},
		{version: 13, sql: migration0013},
		{version: 14, sql: migration0014},
		{version: 15, sql: migration0015},
		{version: 16, sql: migration0016},
		{version: 17, sql: migration0017},
		{version: 18, sql: migration0018},
		{version: 19, sql: migration0019},
	} {
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("版本 %d: %w", m.version, err)
		}
	}
	return nil
}

func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var applied int
	err = tx.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version=?`, m.version).Scan(&applied)
	if err != nil {
		return err
	}
	if applied != 0 {
		return tx.Commit()
	}
	switch m.version {
	case 2:
		hasColumn, err := tableHasColumn(tx, "tokens", "manage_hosts")
		if err != nil {
			return err
		}
		if !hasColumn {
			if _, err = tx.Exec(m.sql); err != nil {
				return err
			}
		}
	case 8:
		hasColumn, err := tableHasColumn(tx, "audit", "token_name")
		if err != nil {
			return err
		}
		if !hasColumn {
			if _, err = tx.Exec(`ALTER TABLE audit ADD COLUMN token_name TEXT`); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(m.sql); err != nil {
			return err
		}
	case 11:
		hasColumn, err := tableHasColumn(tx, "jobs", "log_bytes")
		if err != nil {
			return err
		}
		if !hasColumn {
			if _, err = tx.Exec(`ALTER TABLE jobs ADD COLUMN log_bytes INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(m.sql); err != nil {
			return err
		}
		hasColumn, err = tableHasColumn(tx, "command_runs", "output_cleaned")
		if err != nil {
			return err
		}
		if !hasColumn {
			if _, err = tx.Exec(`ALTER TABLE command_runs ADD COLUMN output_cleaned INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
	case 12:
		hasColumn, err := tableHasColumn(tx, "audit", "command_run_ids_json")
		if err != nil {
			return err
		}
		if !hasColumn {
			if _, err = tx.Exec(m.sql); err != nil {
				return err
			}
		}
	case 13:
		// 逐表判列后再 ALTER：重复执行不会因 "duplicate column name" 让启动失败。
		for _, col := range disabledToolsColumns {
			hasColumn, err := tableHasColumn(tx, col.table, "disabled_tools_json")
			if err != nil {
				return err
			}
			if hasColumn {
				continue
			}
			if _, err = tx.Exec(col.ddl); err != nil {
				return err
			}
		}
	default:
		if _, err = tx.Exec(m.sql); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,strftime('%s','now'))`, m.version); err != nil {
		return err
	}
	return tx.Commit()
}

func tableHasColumn(tx *sql.Tx, table, column string) (bool, error) {
	return hasColumn(tx.Query, table, column)
}

func tableHasColumnDB(db *sql.DB, table, column string) (bool, error) {
	return hasColumn(db.Query, table, column)
}

// isDuplicateColumnError 判断错误是否为"列已存在"。
// modernc.org/sqlite 把它报成 "SQL logic error: duplicate column name: xxx (1)"。
func isDuplicateColumnError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// tableExists 用 pragma_table_info 的空结果判断表是否存在，
// 避免在表缺失时把 "no such table" 误报成缺列。
func tableExists(db *sql.DB, table string) (bool, error) {
	// pragma_table_info 的表名必须是字符串字面量，裸标识符会被当成列名。
	rows, err := db.Query(`SELECT name FROM pragma_table_info('` + table + `') LIMIT 1`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func hasColumn(query func(string, ...any) (*sql.Rows, error), table, column string) (bool, error) {
	rows, err := query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close 关闭数据库。停机前先做一次 TRUNCATE checkpoint 把 WAL 合并回主库文件：
// 否则留在 WAL 里的事务只能在下次打开时回放，进程被强制终止时还可能残留 -wal 与 -shm。
func (s *Store) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if busy, err := s.CheckpointWAL(ctx); err != nil {
		log.Printf("关闭前 WAL checkpoint 失败: %v", err)
	} else if busy {
		log.Printf("关闭前 WAL checkpoint 被其他连接占用，剩余内容将在下次打开时回放")
	}
	return s.DB.Close()
}
func (s *Store) Health(ctx context.Context) error { return s.DB.PingContext(ctx) }
