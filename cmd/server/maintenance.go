package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"path/filepath"
	"strings"
	"time"

	"cups-web/internal/store"
)

func startMaintenance(s *store.Store, uploads string) {
	go func() {
		for {
			if err := cleanupOldPrints(context.Background(), s, uploads, time.Now()); err != nil {
				log.Println("cleanup failed:", err)
			}
			time.Sleep(1 * time.Hour)
		}
	}()
}

func cleanupAllPrints(ctx context.Context, s *store.Store, uploads string) (int, error) {
	var paths []string
	recordCount := 0
	err := s.WithTx(ctx, false, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT stored_path, photo_layout_json, rendered_path FROM print_jobs")
		if err != nil {
			return err
		}
		for rows.Next() {
			var record store.PrintRecord
			var photoJSON string
			if err := rows.Scan(&record.StoredPath, &photoJSON, &record.RenderedPath); err != nil {
				rows.Close()
				return err
			}
			if err := json.Unmarshal([]byte(photoJSON), &record.PhotoLayout); err != nil {
				rows.Close()
				return err
			}
			paths = append(paths, printRecordPaths(record)...)
			recordCount++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		_, err = tx.ExecContext(ctx, "DELETE FROM print_jobs")
		return err
	})
	if err != nil {
		return 0, err
	}

	removePrintAssets(uploads, paths)

	if recordCount > 0 {
		if _, err := s.DB.ExecContext(ctx, "VACUUM"); err != nil {
			return recordCount, err
		}
		if _, err := s.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return recordCount, err
		}
	}
	return recordCount, nil
}

func cleanupOldPrints(ctx context.Context, s *store.Store, uploads string, now time.Time) error {
	var retentionDays int64
	err := s.WithTx(ctx, true, func(tx *sql.Tx) error {
		val, err := store.GetSettingInt(ctx, tx, store.SettingRetentionDays, 0)
		if err != nil {
			return err
		}
		retentionDays = val
		return nil
	})
	if err != nil {
		return err
	}
	if retentionDays <= 0 {
		return nil
	}

	cutoff := now.AddDate(0, 0, -int(retentionDays)).UTC().Format(time.RFC3339)
	var paths []string
	recordCount := 0
	err = s.WithTx(ctx, false, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT stored_path, photo_layout_json, rendered_path FROM print_jobs WHERE created_at < ?", cutoff)
		if err != nil {
			return err
		}
		for rows.Next() {
			var record store.PrintRecord
			var photoJSON string
			if err := rows.Scan(&record.StoredPath, &photoJSON, &record.RenderedPath); err != nil {
				rows.Close()
				return err
			}
			if err := json.Unmarshal([]byte(photoJSON), &record.PhotoLayout); err != nil {
				rows.Close()
				return err
			}
			paths = append(paths, printRecordPaths(record)...)
			recordCount++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		_, err = tx.ExecContext(ctx, "DELETE FROM print_jobs WHERE created_at < ?", cutoff)
		return err
	})
	if err != nil {
		return err
	}

	removePrintAssets(uploads, paths)

	if recordCount > 0 {
		if _, err := s.DB.ExecContext(ctx, "VACUUM"); err != nil {
			return err
		}
		if _, err := s.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return err
		}
	}
	return nil
}

// isScheduledRel 判断一个 stored_path 是否指向定时任务的源文件。
// 前缀与 scheduledSubDir 保持一致，全路径归一化为斜杠再判断，避免跨平台差异。
func isScheduledRel(rel string) bool {
	if rel == "" {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel == scheduledSubDir ||
		strings.HasPrefix(rel, scheduledSubDir+"/")
}
