package store

import (
	"context"
	"testing"
)

// 迁移只按 schema_migrations 里的版本号跳过。若某条 DDL 被记为已应用却没真正生效，
// 库会永久缺列，且再也不会被迁移补回来，运行时直接报 "no such column"。
// 这里模拟这种漂移，验证启动自检能把列补回来。
func TestEnsureRequiredColumnsRepairsDriftedSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateToken(ctx, TokenCreate{Name: "before-drift", Hash: "hash-before", AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}

	// 人为制造漂移：把三张表的列删掉，但保留 schema_migrations 里已记录的 13。
	drifted, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"tokens", "oauth_authorization_codes", "oauth_refresh_tokens"} {
		if _, err = drifted.DB.ExecContext(ctx, `ALTER TABLE `+table+` DROP COLUMN disabled_tools_json`); err != nil {
			t.Fatalf("制造漂移失败 %s: %v", table, err)
		}
	}
	if err = drifted.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：迁移 13 已被标记为 applied 不会重跑，只能靠自检补齐。
	repaired, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer repaired.Close()

	for _, table := range []string{"tokens", "oauth_authorization_codes", "oauth_refresh_tokens"} {
		ok, err := tableHasColumnDB(repaired.DB, table, "disabled_tools_json")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("%s 的 disabled_tools_json 未补建", table)
		}
	}

	// 补列后，用户报告的两条失败路径都必须恢复：列表与新增。
	if _, err = repaired.ListTokensWithHosts(ctx); err != nil {
		t.Fatalf("令牌列表仍失败: %v", err)
	}
	if _, err = repaired.CreateToken(ctx, TokenCreate{
		Name:          "after-repair",
		Hash:          "hash-after",
		AllHosts:      true,
		DisabledTools: []string{"exec"},
	}); err != nil {
		t.Fatalf("新增令牌仍失败: %v", err)
	}

	tokens, err := repaired.ListTokensWithHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("令牌数 = %d, 期望 2", len(tokens))
	}
}

// 自检必须幂等：已经带列的库重复打开不应报错，也不应重复 ALTER。
func TestEnsureRequiredColumnsIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		st, err := Open(dir)
		if err != nil {
			t.Fatalf("第 %d 次打开失败: %v", i+1, err)
		}
		if err = st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
