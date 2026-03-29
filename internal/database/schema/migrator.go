package schema

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/jmoiron/sqlx"
)

//go:embed mysql sqlite
var migrationsFS embed.FS

type taskFile struct {
	Tasks []task `json:"tasks"`
}

type task struct {
	ID          string      `json:"id"`
	SQLFile     string      `json:"sql_file"`
	Description string      `json:"description"`
	Conditions  []condition `json:"conditions"`
}

type condition struct {
	Type   string `json:"type"` // "column_not_exists" | "index_not_exists" | "column_exists" | "index_exists" | "table_not_exists" | "table_exists"
	Table  string `json:"table"`
	Column string `json:"column,omitempty"`
	Index  string `json:"index,omitempty"`
}

// Migrate reads the driver-specific migrations and applies them.
func Migrate(db *sqlx.DB, dbName string, driver string) error {
	empty, err := schemaIsEmpty(db, dbName, driver)
	if err != nil {
		return fmt.Errorf("schema: check empty: %w", err)
	}
	if empty {
		log.Println("[migrate] empty schema detected – running init.sql")
		if err := runInitSQL(db, driver); err != nil {
			return err
		}
	} else {
		log.Println("[migrate] checking if tables exist")
		tableNames := []string{"bug_reports", "bug_report_files", "rate_limit_hwid", "user", "session"}
		var foundTables []string
		for _, tableName := range tableNames {
			found, err := tableExists(db, dbName, tableName, driver)
			if err != nil {
				return fmt.Errorf("schema: check table %s: %w", tableName, err)
			}
			if !found {
				log.Printf("[migrate] warning: expected table %s not found", tableName)
				continue
			}
			foundTables = append(foundTables, tableName)
		}
		if len(foundTables) != len(tableNames) {
			log.Printf("[migrate] warning: expected %d tables, found %d – running init.sql", len(tableNames), len(foundTables))
			if err := runInitSQL(db, driver); err != nil {
				return err
			}
		} else {
			log.Println("[migrate] all expected tables found – skipping init.sql")
		}
	}

	raw, err := migrationsFS.ReadFile(driver + "/migrations/tasks.json")
	if err != nil {
		return fmt.Errorf("schema: read tasks.json: %w", err)
	}

	var tf taskFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return fmt.Errorf("schema: parse tasks.json: %w", err)
	}

	for _, t := range tf.Tasks {
		needed, err := shouldRun(db, dbName, t.Conditions, driver)
		if err != nil {
			return fmt.Errorf("schema: evaluate conditions for %s: %w", t.ID, err)
		}
		if !needed {
			log.Printf("[migrate] skip %s – conditions already met", t.ID)
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile(driver + "/migrations/" + t.SQLFile)
		if err != nil {
			return fmt.Errorf("schema: read %s: %w", t.SQLFile, err)
		}

		stmts := splitStatements(string(sqlBytes))
		for _, stmt := range stmts {
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("schema: exec %s: %w\nSQL: %s", t.ID, err, stmt)
			}
		}
		log.Printf("[migrate] applied %s – %s", t.ID, t.Description)
	}
	return nil
}

func runInitSQL(db *sqlx.DB, driver string) error {
	initSQL, err := migrationsFS.ReadFile(driver + "/init.sql")
	if err != nil {
		return fmt.Errorf("schema: read init.sql: %w", err)
	}
	for _, stmt := range splitStatements(string(initSQL)) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("schema: exec init.sql: %w\nSQL: %s", err, stmt)
		}
	}
	log.Println("[migrate] init.sql applied – base schema created")
	return nil
}

// ---------- Condition evaluator ----------

func shouldRun(db *sqlx.DB, dbName string, conds []condition, driver string) (bool, error) {
	for _, c := range conds {
		met, err := evaluate(db, dbName, c, driver)
		if err != nil {
			return false, err
		}
		if met {
			return true, nil
		}
	}
	return false, nil
}

func evaluate(db *sqlx.DB, dbName string, c condition, driver string) (bool, error) {
	switch c.Type {
	case "column_not_exists":
		exists, err := columnExists(db, dbName, c.Table, c.Column, driver)
		return !exists, err

	case "column_exists":
		return columnExists(db, dbName, c.Table, c.Column, driver)

	case "index_not_exists":
		exists, err := indexExists(db, dbName, c.Table, c.Index, driver)
		return !exists, err

	case "index_exists":
		return indexExists(db, dbName, c.Table, c.Index, driver)

	case "table_not_exists":
		exists, err := tableExists(db, dbName, c.Table, driver)
		return !exists, err

	case "table_exists":
		return tableExists(db, dbName, c.Table, driver)

	default:
		return false, fmt.Errorf("unknown condition type: %s", c.Type)
	}
}

// ---------- MySQL condition checks ----------

func columnExistsMySQL(db *sqlx.DB, dbName, table, column string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		dbName, table, column)
	return count > 0, err
}

func indexExistsMySQL(db *sqlx.DB, dbName, table, index string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ?`,
		dbName, table, index)
	return count > 0, err
}

func tableExistsMySQL(db *sqlx.DB, dbName, table string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`,
		dbName, table)
	return count > 0, err
}

func schemaIsEmptyMySQL(db *sqlx.DB, dbName string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?`, dbName)
	return count == 0, err
}

// ---------- SQLite condition checks ----------

func columnExistsSQLite(db *sqlx.DB, table, column string) (bool, error) {
	var count int
	// pragma_table_info is a table-valued function available since SQLite 3.16.0
	err := db.Get(&count,
		fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = ?", table),
		column)
	return count > 0, err
}

func indexExistsSQLite(db *sqlx.DB, table, index string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name=? AND name=?`,
		table, index)
	return count > 0, err
}

func tableExistsSQLite(db *sqlx.DB, table string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table)
	return count > 0, err
}

func schemaIsEmptySQLite(db *sqlx.DB) (bool, error) {
	var count int
	err := db.Get(&count, `SELECT COUNT(*) FROM sqlite_master WHERE type='table'`)
	return count == 0, err
}

// ---------- Driver-dispatched wrappers ----------

func columnExists(db *sqlx.DB, dbName, table, column, driver string) (bool, error) {
	if driver == "sqlite" {
		return columnExistsSQLite(db, table, column)
	}
	return columnExistsMySQL(db, dbName, table, column)
}

func indexExists(db *sqlx.DB, dbName, table, index, driver string) (bool, error) {
	if driver == "sqlite" {
		return indexExistsSQLite(db, table, index)
	}
	return indexExistsMySQL(db, dbName, table, index)
}

func tableExists(db *sqlx.DB, dbName, table, driver string) (bool, error) {
	if driver == "sqlite" {
		return tableExistsSQLite(db, table)
	}
	return tableExistsMySQL(db, dbName, table)
}

func schemaIsEmpty(db *sqlx.DB, dbName, driver string) (bool, error) {
	if driver == "sqlite" {
		return schemaIsEmptySQLite(db)
	}
	return schemaIsEmptyMySQL(db, dbName)
}

// splitStatements splits a SQL blob on top-level ";" only, respecting
// BEGIN...END blocks (e.g. triggers) so their inner semicolons are not split.
func splitStatements(sql string) []string {
	var out []string
	var buf strings.Builder
	depth := 0
	n := len(sql)

	for i := 0; i < n; {
		c := sql[i]

		// Collect whole identifier tokens to detect BEGIN / END keywords.
		if isIdentStart(c) {
			j := i
			for j < n && isIdentChar(sql[j]) {
				j++
			}
			word := strings.ToUpper(sql[i:j])
			switch word {
			case "BEGIN":
				depth++
			case "END":
				if depth > 0 {
					depth--
				}
			}
			buf.WriteString(sql[i:j])
			i = j
			continue
		}

		if c == ';' && depth == 0 {
			if stmt := strings.TrimSpace(buf.String()); stmt != "" {
				out = append(out, stmt)
			}
			buf.Reset()
			i++
			continue
		}

		buf.WriteByte(c)
		i++
	}

	if stmt := strings.TrimSpace(buf.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}
