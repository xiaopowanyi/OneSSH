package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
)

const (
	metricDisksGZIPPrefix = "\x00ONESSH-DISKS-GZIP-1\x00"
	metricStorageBatch    = 1000
	maxMetricDisksJSON    = 16 << 20
)

type metricDiskRow struct {
	hostID int64
	ts     int64
	disks  sql.NullString
}

// encodeMetricDisks stores large disk snapshots as a gzip BLOB. Small values
// remain plain text because a gzip header would make them larger.
func encodeMetricDisks(raw string) (any, bool, error) {
	if raw == "" {
		return raw, false, nil
	}
	if strings.HasPrefix(raw, metricDisksGZIPPrefix) {
		return []byte(raw), false, nil
	}
	var packed bytes.Buffer
	packed.WriteString(metricDisksGZIPPrefix)
	w, err := gzip.NewWriterLevel(&packed, gzip.BestSpeed)
	if err != nil {
		return nil, false, err
	}
	if _, err = w.Write([]byte(raw)); err != nil {
		_ = w.Close()
		return nil, false, err
	}
	if err = w.Close(); err != nil {
		return nil, false, err
	}
	if packed.Len() >= len(raw) {
		return raw, false, nil
	}
	return packed.Bytes(), true, nil
}

func decodeMetricDisks(stored string) (string, error) {
	if !strings.HasPrefix(stored, metricDisksGZIPPrefix) {
		return stored, nil
	}
	r, err := gzip.NewReader(strings.NewReader(stored[len(metricDisksGZIPPrefix):]))
	if err != nil {
		return "", fmt.Errorf("打开压缩的磁盘指标: %w", err)
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, maxMetricDisksJSON+1))
	if err != nil {
		return "", fmt.Errorf("读取压缩的磁盘指标: %w", err)
	}
	if len(data) > maxMetricDisksJSON {
		return "", fmt.Errorf("解压后的磁盘指标超过 %d 字节", maxMetricDisksJSON)
	}
	return string(data), nil
}

// CompactMetricStorage gradually converts old disk snapshots, then runs VACUUM
// only when the conversion has freed a meaningful number of SQLite pages.
// The cursor is committed with each batch so an interrupted conversion resumes
// safely on the next maintenance pass.
func (s *Store) CompactMetricStorage(ctx context.Context) error {
	var compressed int64
	for {
		count, done, err := s.compressMetricDiskBatch(ctx, metricStorageBatch)
		if err != nil {
			return err
		}
		compressed += count
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if compressed > 0 {
		log.Printf("已压缩 %d 条历史磁盘指标", compressed)
	}
	return s.compactMetricDatabase(ctx)
}

func (s *Store) compressMetricDiskBatch(ctx context.Context, limit int) (compressed int64, done bool, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var hostCursor, tsCursor int64
	var finished int
	if err = tx.QueryRowContext(ctx, `SELECT cursor_host_id,cursor_ts,compression_done
		FROM metric_disk_storage_state WHERE id=1`).Scan(&hostCursor, &tsCursor, &finished); err != nil {
		return 0, false, err
	}
	if finished != 0 {
		return 0, true, tx.Commit()
	}

	rows, err := tx.QueryContext(ctx, `SELECT host_id,ts,disks_json FROM metrics
		WHERE host_id>? OR (host_id=? AND ts>?) ORDER BY host_id,ts LIMIT ?`,
		hostCursor, hostCursor, tsCursor, limit)
	if err != nil {
		return 0, false, err
	}
	batch := make([]metricDiskRow, 0, limit)
	for rows.Next() {
		var item metricDiskRow
		if err = rows.Scan(&item.hostID, &item.ts, &item.disks); err != nil {
			rows.Close()
			return 0, false, err
		}
		batch = append(batch, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, false, err
	}
	if err = rows.Close(); err != nil {
		return 0, false, err
	}
	if len(batch) == 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE metric_disk_storage_state SET compression_done=1 WHERE id=1`); err != nil {
			return 0, false, err
		}
		if err = tx.Commit(); err != nil {
			return 0, false, err
		}
		return 0, true, nil
	}

	update, err := tx.PrepareContext(ctx, `UPDATE metrics SET disks_json=? WHERE host_id=? AND ts=?`)
	if err != nil {
		return 0, false, err
	}
	for _, item := range batch {
		if !item.disks.Valid || item.disks.String == "" || strings.HasPrefix(item.disks.String, metricDisksGZIPPrefix) {
			continue
		}
		stored, changed, encodeErr := encodeMetricDisks(item.disks.String)
		if encodeErr != nil {
			_ = update.Close()
			return 0, false, encodeErr
		}
		if !changed {
			continue
		}
		if _, err = update.ExecContext(ctx, stored, item.hostID, item.ts); err != nil {
			_ = update.Close()
			return 0, false, err
		}
		compressed++
	}
	if err = update.Close(); err != nil {
		return 0, false, err
	}
	last := batch[len(batch)-1]
	if _, err = tx.ExecContext(ctx, `UPDATE metric_disk_storage_state
		SET cursor_host_id=?,cursor_ts=? WHERE id=1`, last.hostID, last.ts); err != nil {
		return 0, false, err
	}
	if err = tx.Commit(); err != nil {
		return 0, false, err
	}
	return compressed, false, nil
}

func (s *Store) compactMetricDatabase(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var finished, compacted int
	if err := s.DB.QueryRowContext(ctx, `SELECT compression_done,compaction_done
		FROM metric_disk_storage_state WHERE id=1`).Scan(&finished, &compacted); err != nil {
		return err
	}
	if finished == 0 || compacted != 0 {
		return nil
	}

	var pageCount, freePages int64
	if err := s.DB.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return err
	}
	if err := s.DB.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&freePages); err != nil {
		return err
	}
	if freePages < max(int64(256), pageCount/20) {
		_, err := s.DB.ExecContext(ctx, `UPDATE metric_disk_storage_state SET compaction_done=1 WHERE id=1`)
		return err
	}
	if busy, err := s.CheckpointWAL(ctx); err != nil {
		return err
	} else if busy {
		return fmt.Errorf("SQLite checkpoint 正忙，稍后重试压缩数据库")
	}
	if _, err := s.DB.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("压缩 SQLite 数据库: %w", err)
	}
	var compactedPages int64
	if err := s.DB.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&compactedPages); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE metric_disk_storage_state SET compaction_done=1 WHERE id=1`); err != nil {
		return err
	}
	if compactedPages < pageCount {
		log.Printf("SQLite 数据库压缩完成，回收约 %d MB", (pageCount-compactedPages)*4096/(1024*1024))
	}
	return nil
}
