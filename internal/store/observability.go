package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MaxAuditFilterValues 将单个筛选维度限制在 SQLite 常见变量上限以内。
// token 条件每项占 3 个变量；三个维度各 100 项时总计不超过 500。
const MaxAuditFilterValues = 100

// AuditRetention 是审计记录的保留时长。审计是安全追溯的主要依据，保留期刻意长于
// 指标、命令输出与 artifact（均为 7 天）。在此之上刻意不提供「永不过期」选项：
// 审计表每次 MCP 调用都会写入，没有上界时会一直增长到拖垮整个数据目录。
const AuditRetention = 90 * 24 * time.Hour

// CleanupAudit 删除保留期之前的审计记录并返回删除行数。audit_ts 索引让这条 DELETE
// 直接命中时间范围，不会变成全表扫描。
func (s *Store) CleanupAudit(ctx context.Context, cutoff int64) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.DB.ExecContext(ctx, `DELETE FROM audit WHERE ts<?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) AddAudit(ctx context.Context, a Audit) error {
	var runIDs any
	if len(a.RunIDs) > 0 {
		encoded, err := json.Marshal(a.RunIDs)
		if err != nil {
			return fmt.Errorf("编码命令执行关联: %w", err)
		}
		runIDs = string(encoded)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit(ts,token_id,token_name,tool,host,params_json,command_run_ids_json,ok,exit_code,duration_ms,bytes_out) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a.Ts, nullInt(a.TokenID), nullString(a.TokenName), a.Tool, nullString(a.Host), a.ParamsJSON, runIDs, boolInt(a.OK), nullInt(a.ExitCode), a.DurationMS, a.BytesOut)
	return err
}

func validateAuditFilterCount(name string, count int) error {
	if count > MaxAuditFilterValues {
		return fmt.Errorf("审计%s筛选最多 %d 项", name, MaxAuditFilterValues)
	}
	return nil
}
func (s *Store) ListAudit(ctx context.Context, tokenIDs []int64, hosts, tools []string, ok *bool, before int64, limit int) ([]Audit, error) {
	if err := validateAuditFilterCount("令牌", len(tokenIDs)); err != nil {
		return nil, err
	}
	if err := validateAuditFilterCount("主机", len(hosts)); err != nil {
		return nil, err
	}
	if err := validateAuditFilterCount("工具", len(tools)); err != nil {
		return nil, err
	}
	q := `SELECT id,ts,token_id,token_name,tool,host,params_json,command_run_ids_json,ok,exit_code,duration_ms,bytes_out FROM audit WHERE 1=1`
	args := []any{}
	if len(tokenIDs) > 0 {
		// 迁移前的 ID 可能已被复用；当前令牌存在时，只接受写入时已快照其名称的记录。
		// 多值是 OR 关系：任一 id 命中即保留，每个 id 独立做名称快照校验。
		cond := `audit.token_id=? AND (
			NOT EXISTS (SELECT 1 FROM tokens WHERE tokens.id=?)
			OR EXISTS (
				SELECT 1 FROM tokens
				WHERE tokens.id=? AND audit.token_name=tokens.name
			)
		)`
		conds := make([]string, len(tokenIDs))
		for i, id := range tokenIDs {
			conds[i] = `(` + cond + `)`
			args = append(args, id, id, id)
		}
		q += ` AND (` + strings.Join(conds, ` OR `) + `)`
	}
	if len(hosts) > 0 {
		q += ` AND host IN (` + placeholders(len(hosts)) + `)`
		for _, h := range hosts {
			args = append(args, h)
		}
	}
	if len(tools) > 0 {
		q += ` AND tool IN (` + placeholders(len(tools)) + `)`
		for _, t := range tools {
			args = append(args, t)
		}
	}
	if ok != nil {
		q += ` AND ok=?`
		args = append(args, boolInt(*ok))
	}
	if before > 0 {
		q += ` AND id<?`
		args = append(args, before)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Audit, 0)
	for rows.Next() {
		var a Audit
		var ok int
		var runIDs sql.NullString
		if err := rows.Scan(&a.ID, &a.Ts, &a.TokenID, &a.TokenName, &a.Tool, &a.Host, &a.ParamsJSON, &runIDs, &ok, &a.ExitCode, &a.DurationMS, &a.BytesOut); err != nil {
			return nil, err
		}
		if runIDs.Valid {
			if err := json.Unmarshal([]byte(runIDs.String), &a.RunIDs); err != nil {
				return nil, fmt.Errorf("解析审计命令执行关联: %w", err)
			}
		}
		a.OK = ok != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListAuditTools(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT tool FROM audit WHERE tool<>'' ORDER BY tool`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tools := make([]string, 0)
	for rows.Next() {
		var tool string
		if err := rows.Scan(&tool); err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, rows.Err()
}
func (s *Store) AddMetric(ctx context.Context, m Metric) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.DB.ExecContext(ctx, `INSERT OR REPLACE INTO metrics(host_id,ts,cpu_pct,mem_used_kb,mem_total_kb,load1,disks_json) VALUES(?,?,?,?,?,?,?)`, m.HostID, m.Ts, nullFloat(m.CPUPct), nullInt(m.MemUsedKB), nullInt(m.MemTotalKB), nullFloat(m.Load1), m.DisksJSON)
	return err
}
func (s *Store) LatestMetric(ctx context.Context, hostID int64) (Metric, error) {
	var m Metric
	err := s.DB.QueryRowContext(ctx, `SELECT host_id,ts,cpu_pct,mem_used_kb,mem_total_kb,load1,disks_json FROM metrics WHERE host_id=? ORDER BY ts DESC LIMIT 1`, hostID).Scan(&m.HostID, &m.Ts, &m.CPUPct, &m.MemUsedKB, &m.MemTotalKB, &m.Load1, &m.DisksJSON)
	return m, err
}
func (s *Store) MetricsSince(ctx context.Context, hostID, since int64) ([]Metric, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT host_id,ts,cpu_pct,mem_used_kb,mem_total_kb,load1,disks_json FROM metrics WHERE host_id=? AND ts>=? ORDER BY ts`, hostID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Metric, 0)
	for rows.Next() {
		var m Metric
		if err := rows.Scan(&m.HostID, &m.Ts, &m.CPUPct, &m.MemUsedKB, &m.MemTotalKB, &m.Load1, &m.DisksJSON); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) CleanupMetrics(ctx context.Context, cutoff int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.DB.ExecContext(ctx, `DELETE FROM metrics WHERE ts<?`, cutoff)
	return err
}
func nullString(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}

// placeholders 生成 IN 子句的 ? 占位串：placeholders(3) → "?,?,?"
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
func nullFloat(v sql.NullFloat64) any {
	if v.Valid {
		return v.Float64
	}
	return nil
}
func NowAudit() int64 { return time.Now().UnixMilli() }
