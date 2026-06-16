package storage

import (
	"bytes"
	"context"
	"database/sql"
	"emly-api-go/internal/models"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

func MigrateReportFilesToS3(db *sqlx.DB, s3conn *S3Connector, dbName string) error {
	var wg sync.WaitGroup
	errCh := make(chan error, 128) // buffer ragionevole
	reportsRows, err := db.Query(fmt.Sprintf("SELECT id, created_at, updated_at FROM %s.bug_reports ORDER BY created_at DESC", dbName))
	if err != nil {
		return err
	}
	defer reportsRows.Close()

	var totalReports, totalFiles, skipped, uploaded int

	for reportsRows.Next() {
		var reportId int
		var createdAt, updatedAt time.Time

		if err := reportsRows.Scan(
			&reportId, &createdAt, &updatedAt,
		); err != nil {
			return err
		}
		totalReports++
		slog.Info("migrate: processing report", "report_id", reportId)

		filesRows, err := db.Query(
			fmt.Sprintf("SELECT id, report_id, filename FROM %s.bug_report_files WHERE report_id = ?", dbName),
			reportId,
		)

		if err != nil {
			return err
		}

		for filesRows.Next() {
			var fileID int
			var fileReportID int
			var fileName string
			if err := filesRows.Scan(&fileID, &fileReportID, &fileName); err != nil {
				filesRows.Close()
				return err
			}

			var file models.BugReportFile
			err := db.GetContext(context.Background(), &file, fmt.Sprintf("SELECT filename, mime_type, data FROM %s.bug_report_files WHERE report_id = ? AND id = ?", dbName), reportId, fileID)
			if errors.Is(err, sql.ErrNoRows) {
				slog.Info("migrate: file not found, skipping", "report_id", reportId, "file_id", fileID)
				skipped++
				continue
			}
			if err != nil {
				filesRows.Close()
				return fmt.Errorf("report %d / file %d: %w", reportId, fileID, err)
			}

			if s3conn != nil {
				s3Key := fmt.Sprintf("emly-api-files/bug-reports/%d/files/%s", reportId, fileName)
				dataCopy := make([]byte, len(file.Data))
				copy(dataCopy, file.Data)
				mime := file.MimeType
				fname := file.Filename
				totalFiles++
				slog.Info("migrate: uploading to s3", "report_id", reportId, "file_id", fileID, "filename", fname, "size_bytes", len(dataCopy), "key", s3Key)
				wg.Add(1)
				go func(key, mimeType, filename string, payload []byte, rid, fid int) {
					defer wg.Done()

					_, upErr := s3conn.UploadFile(
						context.Background(),
						key,
						bytes.NewReader(payload),
						mimeType,
						map[string]string{"filename": filename},
					)
					if upErr != nil {
						errCh <- fmt.Errorf("report %d / file %d (%s): %w", rid, fid, key, upErr)
						slog.Error("migrate: s3 upload failed", "key", key, "err", upErr)
						return
					}
					slog.Info("migrate: s3 upload complete", "key", key)
				}(s3Key, mime, fname, dataCopy, reportId, fileID)

				uploaded++
			}
		}

		if err := filesRows.Close(); err != nil {
			return err
		}
	}

	wg.Wait()
	close(errCh)

	var uploadErrCount int
	for e := range errCh {
		uploadErrCount++
		slog.Error("migrate: upload error", "err", e)
	}

	slog.Info("migrate: done", "reports", totalReports, "files_queued", uploaded, "skipped", skipped, "upload_errors", uploadErrCount)

	if uploadErrCount > 0 {
		return fmt.Errorf("migration completed with %d upload errors", uploadErrCount)
	}
	return nil
}
