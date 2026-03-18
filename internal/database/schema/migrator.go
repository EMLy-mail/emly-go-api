package schema

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/jmoiron/sqlx"
)

//go:embed init.sql migrations/*.json migrations/*.sql
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

// Migrate reads migrations/tasks.json and executes every task whose
// conditions are ALL satisfied (i.e. logical AND).
func Migrate(db *sqlx.DB, dbName string) error {
	// If the database has no tables at all, bootstrap with init.sql.
	empty, err := schemaIsEmpty(db, dbName)
	if err != nil {
		return fmt.Errorf("schema: check empty: %w", err)
	}
	if empty {
		log.Println("[migrate] empty schema detected – running init.sql")
		initSQL, err := migrationsFS.ReadFile("init.sql")
		if err != nil {
			return fmt.Errorf("schema: read init.sql: %w", err)
		}
		for _, stmt := range splitStatements(string(initSQL)) {
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("schema: exec init.sql: %w\nSQL: %s", err, stmt)
			}
		}
		log.Println("[migrate] init.sql applied – base schema created")
	} else {
		log.Println("[migrate] checking if tables exist")
		// Check if the tables are there or not
		var tableNames []string
		var foundTables []string
		tableNames = append(tableNames, "bug_reports", "bug_report_files", "rate_limit_hwid", "user", "session")
		for _, tableName := range tableNames {
			found, err := tableExists(db, dbName, tableName)
			if err != nil {
				return fmt.Errorf("schema: check table %s: %w", tableName, err)
			}
			if !found {
				log.Printf("[migrate] warning: expected table %s not found – schema may be in an inconsistent state", tableName)
				continue
			}
			foundTables = append(foundTables, tableName)
		}
		if len(foundTables) != len(tableNames) {
			log.Printf("[migrate] warning: expected %d tables, found %d", len(tableNames), len(foundTables))
			log.Printf("[migrate] info: running init.sql")
			initSQL, err := migrationsFS.ReadFile("init.sql")
			if err != nil {
				return fmt.Errorf("schema: read init.sql: %w", err)
			}
			for _, stmt := range splitStatements(string(initSQL)) {
				if _, err := db.Exec(stmt); err != nil {
					return fmt.Errorf("schema: exec init.sql: %w\nSQL: %s", err, stmt)
				}
			}
			log.Println("[migrate] init.sql applied – base schema created")
		} else {
			log.Println("[migrate] all expected tables found – skipping init.sql")
		}
	}

	raw, err := migrationsFS.ReadFile("migrations/tasks.json")
	if err != nil {
		return fmt.Errorf("schema: read tasks.json: %w", err)
	}

	var tf taskFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return fmt.Errorf("schema: parse tasks.json: %w", err)
	}

	for _, t := range tf.Tasks {
		needed, err := shouldRun(db, dbName, t.Conditions)
		if err != nil {
			return fmt.Errorf("schema: evaluate conditions for %s: %w", t.ID, err)
		}
		if !needed {
			log.Printf("[migrate] skip %s – conditions already met", t.ID)
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + t.SQLFile)
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

// ---------- Condition evaluator ----------

func shouldRun(db *sqlx.DB, dbName string, conds []condition) (bool, error) {
	for _, c := range conds {
		met, err := evaluate(db, dbName, c)
		if err != nil {
			return false, err
		}
		if met {
			return true, nil
		}
	}
	return false, nil
}

func evaluate(db *sqlx.DB, dbName string, c condition) (bool, error) {
	switch c.Type {
	case "column_not_exists":
		exists, err := columnExists(db, dbName, c.Table, c.Column)
		return !exists, err

	case "column_exists":
		return columnExists(db, dbName, c.Table, c.Column)

	case "index_not_exists":
		exists, err := indexExists(db, dbName, c.Table, c.Index)
		return !exists, err

	case "index_exists":
		return indexExists(db, dbName, c.Table, c.Index)

	case "table_not_exists":
		exists, err := tableExists(db, dbName, c.Table)
		return !exists, err

	case "table_exists":
		return tableExists(db, dbName, c.Table)

	default:
		return false, fmt.Errorf("unknown condition type: %s", c.Type)
	}
}

// ---------- MySQL introspection helpers ----------

func columnExists(db *sqlx.DB, dbName, table, column string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = ?
		   AND TABLE_NAME   = ?
		   AND COLUMN_NAME  = ?`, dbName, table, column)
	return count > 0, err
}

func indexExists(db *sqlx.DB, dbName, table, index string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = ?
		   AND TABLE_NAME   = ?
		   AND INDEX_NAME   = ?`, dbName, table, index)
	return count > 0, err
}

func tableExists(db *sqlx.DB, dbName, table string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = ?
		   AND TABLE_NAME   = ?`, dbName, table)
	return count > 0, err
}

func schemaIsEmpty(db *sqlx.DB, dbName string) (bool, error) {
	var count int
	err := db.Get(&count,
		`SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = ?`, dbName)
	return count == 0, err
}

// splitStatements splits a SQL blob on ";" respecting only top-level
// semicolons (good enough for simple ALTER / CREATE statements).
func splitStatements(sql string) []string {
	raw := strings.Split(sql, ";")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
