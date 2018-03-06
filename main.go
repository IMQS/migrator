package main

/*

-----------------------------------------------
Testing the Albion switchover case on an old DB
-----------------------------------------------
CREATE DATABASE legacydb;
\c legacydb
CREATE TABLE schema_migrations (version INTEGER);
INSERT INTO schema_migrations (version) VALUES (1),(56); -- Failure case, where the Albion database doesn't yet have 57, so we can't proceed.
INSERT INTO schema_migrations (version) VALUES (57);     -- With this addition, we can proceed

vgo build && ./migrator logs postgres:localhost:0:legacydb:unit_test_user:unit_test_password example-legacydb
# Verify that only the 2018- transactions have run

----------------------------------------------
Testing the Albion switchover case on a new DB
----------------------------------------------
vgo build && ./migrator logs postgres:localhost:0:newdb:unit_test_user:unit_test_password example-legacydb
# Verify that all 3 transactions have run (the 0000- transactions, and the 2018- transactions)

--------------------------------------------------------
Testing a new database that never used the Albion system
--------------------------------------------------------
vgo build && ./migrator logs postgres:localhost:0:newdb:unit_test_user:unit_test_password example-newdb
# Verify that both transactions have run

*/

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/IMQS/log"
	_ "github.com/lib/pq"
)

const metaTableCreateStatement = "CREATE TABLE schema_migrations (version VARCHAR PRIMARY KEY);"

type dbCon struct {
	driver   string
	host     string
	port     string
	dbname   string
	user     string
	password string
	sslmode  string
}

func (c *dbCon) makeConStr() string {
	s := fmt.Sprintf("host=%v dbname=%v user=%v", c.host, c.dbname, c.user)
	if c.port != "0" && c.port != "" {
		s += fmt.Sprintf(" port=%v", c.port)
	}
	if c.password != "" {
		s += fmt.Sprintf(" password=%v", c.password)
	}
	if c.sslmode != "" {
		s += fmt.Sprintf(" sslmode=%v", c.sslmode)
	} else {
		s += " sslmode=disable"
	}
	return s
}

func (c *dbCon) string() string {
	return c.host + ":" + c.dbname
}

// postgres:hostname:port:dbname:username:password
// port and password may be blank, in which case they are omitted from the connection string
// Returns driver, con, error
func parseDBConStr(dbStr string) (dbCon, error) {
	parts := strings.Split(dbStr, ":")
	if len(parts) != 6 {
		return dbCon{}, fmt.Errorf("Invalid db connection string. Expected 6 colon-separated parts, but only got %v parts", len(parts))
	}
	c := dbCon{
		driver:   parts[0],
		host:     parts[1],
		port:     parts[2],
		dbname:   parts[3],
		user:     parts[4],
		password: parts[5],
	}
	if c.driver != "postgres" {
		return c, fmt.Errorf("Postgres is the only supported database (not %v)", c.driver)
	}
	return c, nil
}

func connectOrCreate(log *log.Logger, con dbCon) (*sql.DB, error) {
	db, err := sql.Open(con.driver, con.makeConStr())
	if err == nil {
		// Try a dummy query, to kick the driver into action
		if _, err := db.Exec("SELECT 1"); err == nil {
			log.Debugf("Connected to database %v", con.dbname)
			return db, nil
		}
	}

	root := con
	root.dbname = "postgres"
	db, err = sql.Open(root.driver, root.makeConStr())
	if err != nil {
		return nil, fmt.Errorf("Failed to connect to database '%v', in order to create database %v: %v", root.string(), con.string(), err)
	}
	log.Infof("Creating database %v", con.dbname)
	_, err = db.Exec(fmt.Sprintf("CREATE DATABASE %v", con.dbname))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("Failed to create database %v: %v", con.string(), err)
	}

	// disconnect from "postgres", and connect to our new DB
	db.Close()
	db, err = sql.Open(con.driver, con.makeConStr())
	if err != nil {
		return nil, fmt.Errorf("Failed to connect to newly created database '%v': %v", con.string(), err)
	}
	/*
		// >>> Let's rather do this with a migration
		// Installing PostGIS doesn't need to be part of this initialization. It could be performed by
		// a migration. But for what this thing was built for, it's convenient to do it here.
		log.Infof("Installing PostGIS in database %v", con.dbname)
		_, err = db.Exec("CREATE EXTENSION postgis")
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("Failed to install postgis into %v: %v", con.string(), err)
		}
	*/

	return db, nil
}

// Detect the presence of the old Albion-based migration system, and take over from that.
// Also support initializing a fresh database.
func bootstrap(log *log.Logger, db *sql.DB, sqlFiles []string) error {
	// Detect the state of this database
	vertype := ""
	if err := db.QueryRow("SELECT data_type FROM information_schema.columns WHERE table_name = 'schema_migrations' AND column_name = 'version'").Scan(&vertype); err != nil {
		if err == sql.ErrNoRows {
			// This is a fresh database
			log.Infof("Initializing new database")
			if _, err := db.Exec(metaTableCreateStatement); err != nil {
				return err
			}
			log.Infof("Running legacy migrations (ie 0000-*.sql)")
			return runLegacyMigrations(log, db, sqlFiles)
		}
		return fmt.Errorf("Unable to read datatype of schema_migrations.version field: %v", err)
	}

	if strings.Index(vertype, "char") != -1 {
		// The database is already using this migration system, so we have no bootstrapping work to do here
		return nil
	}

	// This is a legacy database, controlled by the Albion migration system.
	// Switch over to our new system.
	return switchoverFromAlbion(log, db, sqlFiles)
}

func getMigrationsInDB(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query("SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	versions := map[string]bool{}
	defer rows.Close()
	for rows.Next() {
		version := ""
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		versions[version] = true
	}
	rows.Close()
	return versions, nil
}

// Run the migrations necessary to bring this database up to date with the state that it was in before
// we switched over to this new migration system. For example, for the IMQS 'main' database, we switched
// over from the Albion-based migration system, to this system, somewhere around version 160.
// This function is here to bring a fresh database up to that "160" state.
func runLegacyMigrations(log *log.Logger, db *sql.DB, sqlFiles []string) error {
	for _, file := range sqlFiles {
		if _, isLegacy := legacyMigrationVersion(file); isLegacy {
			if err := runMigration(log, db, file); err != nil {
				return err
			}
		}
	}
	return nil
}

// Scan through all migrations, and return the legacy migration with the highest number
// eg
// 0000-0001.sql
// 0000-0057.sql
// -------------> returns 57
// If there are no legacy migrations, returns zero
func maxLegacyMigrationVersion(sqlFiles []string) int {
	m := 0
	for _, file := range sqlFiles {
		if v, isLegacy := legacyMigrationVersion(file); isLegacy {
			if v > m {
				m = v
			}
		}
	}
	return m
}

// Reads a migration filename, and interprets it as a legacy migration version
func legacyMigrationVersion(sqlfile string) (version int, isLegacy bool) {
	s := filepath.Base(sqlfile)
	s = s[:len(s)-4]
	parts := strings.Split(s, "-")
	if len(parts) != 2 || parts[0] != "0000" || len(parts[1]) == 0 {
		return 0, false
	}
	v, _ := strconv.ParseInt(parts[1], 10, 32)
	return int(v), true
}

func switchoverFromAlbion(log *log.Logger, db *sql.DB, sqlFiles []string) error {
	log.Infof("Switching over from Albion migration system")

	// Make sure the database has been fully migrated on the Albion system, before taking control.
	maxDB := 0
	if err := db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&maxDB); err != nil {
		return fmt.Errorf("Unable to read max legacy version: %v", err)
	}
	maxAvailable := maxLegacyMigrationVersion(sqlFiles)
	if maxDB != maxAvailable {
		return fmt.Errorf("Unable to upgrade migration system. Expected database to be at migration %v, but it is at %v", maxAvailable, maxDB)
	}

	log.Infof("Found legacy DB version %v", maxDB)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DROP TABLE schema_migrations; " +
		metaTableCreateStatement); err != nil {
		tx.Rollback()
		return err
	}
	log.Info("Inserting legacy migrations into schema_migrations (without running them)")
	for _, file := range sqlFiles {
		if _, isLegacy := legacyMigrationVersion(file); isLegacy {
			if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES ($1)", migrationNameFromFile(file)); err != nil {
				tx.Rollback()
				return err
			}

		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Info("Albion switchover complete")
	return nil
}

func migrationNameFromFile(filename string) string {
	name := filepath.Base(filename) // remove directory name
	name = name[0 : len(name)-4]    // remove .sql
	return strings.ToLower(name)
}

func runMigration(log *log.Logger, db *sql.DB, sqlFile string) error {
	sql, err := ioutil.ReadFile(sqlFile)
	if err != nil {
		return fmt.Errorf("Error reading migration file %v: %v", sqlFile, err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	migname := migrationNameFromFile(sqlFile)
	log.Infof("Running migration %v", migname)
	fmt.Printf("Running migration %v\n", migname)
	if _, err := tx.Exec(string(sql)); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES ($1)", migname); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func runMigrations(log *log.Logger, dbStr string, sqlFiles []string) error {
	con, err := parseDBConStr(dbStr)
	if err != nil {
		return err
	}

	db, err := connectOrCreate(log, con)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := bootstrap(log, db, sqlFiles); err != nil {
		return err
	}

	existing, err := getMigrationsInDB(db)
	if err != nil {
		return err
	}
	nrun := 0
	for _, file := range sqlFiles {
		migname := migrationNameFromFile(file)
		if !existing[migname] {
			nrun++
			if err := runMigration(log, db, file); err != nil {
				return err
			}
		}
	}
	if nrun == 0 {
		fmt.Printf("Database is up to date\n")
	}
	return nil
}

func showHelp() {
	fmt.Printf("migrator <logfile> <db> <path to sql files>\n")
}

func main() {
	if len(os.Args) != 4 {
		showHelp()
		os.Exit(1)
	}
	logfile := os.Args[1]
	db := os.Args[2]
	sqlDir := os.Args[3]
	logger := log.New(logfile)
	//logger.Level = log.Debug
	sqlFiles := []string{}
	err := filepath.Walk(sqlDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("Error scanning SQL files: %v", err)
		}
		if filepath.Ext(path) == ".sql" {
			sqlFiles = append(sqlFiles, path)
		}
		if info.IsDir() && path != sqlDir {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		fmt.Printf("Error scanning %v: %v\n", sqlDir, err)
		os.Exit(1)
	}
	if len(sqlFiles) == 0 {
		fmt.Printf("No SQL files found in %v\n", sqlDir)
		os.Exit(1)
	}
	err = runMigrations(logger, db, sqlFiles)
	if err != nil {
		logger.Errorf("%v", err)
		fmt.Printf("%v\n", err)
		os.Exit(1)
	}
}
