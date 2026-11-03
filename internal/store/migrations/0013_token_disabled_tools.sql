-- 运行时权威 DDL 见 internal/store/store.go 的 disabledToolsColumns：
-- 迁移按版本号只跑一次，启动时还会按实际表结构对账补列，两者必须保持一致。
ALTER TABLE tokens ADD COLUMN disabled_tools_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE oauth_authorization_codes ADD COLUMN disabled_tools_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE oauth_refresh_tokens ADD COLUMN disabled_tools_json TEXT NOT NULL DEFAULT '[]';
