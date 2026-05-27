package storage

import (
	"bytes"
	"context"
	"database/sql"
	"emly-api-go/internal/models"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

func MigrateReportFilesToS3(db *sqlx.DB, s3conn *S3Connector, dbName string) error {
	var wg sync.WaitGroup
	errCh := make(chan error, 128) // buffer ragionevole
	reportsRows, err := db.Query("SELECT id, created_at, updated_at FROM emly_bugreports_dev.bug_reports ORDER BY created_at DESC")
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
		log.Printf("[migrate] processing report %d", reportId)

		filesRows, err := db.Query(
			"SELECT id, report_id, filename FROM emly_bugreports_dev.bug_report_files WHERE report_id = ?",
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
				log.Printf("[migrate] report %d / file %d: not found in bug_report_files, skipping", reportId, fileID)
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
				log.Printf("[migrate] report %d / file %d (%s, %d bytes): uploading to s3://%s", reportId, fileID, fname, len(dataCopy), s3Key)
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
						log.Printf("[migrate] [ERROR] upload failed for s3://%s: %v", key, upErr)
						return
					}
					log.Printf("[migrate] upload complete: s3://%s", key)
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
		log.Printf("[migrate] [ERROR] %v", e)
	}

	log.Printf("[migrate] done — reports: %d, files queued: %d, skipped: %d, upload errors: %d",
		totalReports, uploaded, skipped, uploadErrCount)

	if uploadErrCount > 0 {
		return fmt.Errorf("migration completed with %d upload errors", uploadErrCount)
	}
	return nil
}
