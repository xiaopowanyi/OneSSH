-- 补齐热路径缺失的索引。只新增索引，不改表结构，对已有数据无副作用。

-- audit：ListAudit 支持按 token_id / host 过滤，但排序固定为 id DESC。既有的 audit_ts
-- 只能按时间定位，这两个新索引让「先命中过滤条件、再按 id 倒序输出」可以直接走索引，
-- 不必在主键上倒扫并逐条丢弃不匹配的记录。审计表只增不减，省下的扫描量随时间线性增长。
-- 代价是每次写审计多维护两棵 B 树；审计的写入频率远低于它的读取与清理代价。
CREATE INDEX IF NOT EXISTS idx_audit_token_id ON audit(token_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_host ON audit(host, id DESC);

-- jobs：删除主机时同一个写事务里既要统计运行中的任务又要删除任务记录，此前两处都在全表扫描。
-- 缩短它等于缩短 WAL 写锁的持有时间，收益不止于这一个操作。
CREATE INDEX IF NOT EXISTS idx_jobs_host_id ON jobs(host_id);

-- oauth_refresh_tokens：更新令牌权限、删除令牌、吊销授权族都按 access_token_id 定位。
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_access_token ON oauth_refresh_tokens(access_token_id);

-- command_runs：每小时一次的过期输出清理。查询条件里的 job_id IS NULL 是同步命令语义，
-- 用部分索引把索引体积限制在这些行上，避免为后台任务的日志行付代价。
CREATE INDEX IF NOT EXISTS idx_command_runs_expire ON command_runs(finished_at) WHERE job_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_command_runs_clean ON command_runs(id) WHERE job_id IS NULL AND output_cleaned=0;
