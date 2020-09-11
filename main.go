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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/IMQS/log"
	_ "github.com/lib/pq"
)

const metaTableCreateStatement = "CREATE TABLE schema_migrations (version VARCHAR PRIMARY KEY);"
const migrationsRoot = "/dbschema/migrations" // This path is controlled by https://github.com/IMQS/migrations/blob/master/Dockerfile

var validDBNameRegex = regexp.MustCompile(`^[_\-a-zA-Z0-9]+$`)
var validSchemaNameRegex = regexp.MustCompile(`^[_\-a-zA-Z0-9]+$`)

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
func bootstrap(log *log.Logger, dbName string, db *sql.DB, sqlFiles []string) error {
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
	log.Infof("Switching %v over from Albion migration system", dbName)
	fmt.Printf("Switching %v over from Albion migration system\n", dbName)
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

	if err := bootstrap(log, con.dbname, db, sqlFiles); err != nil {
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

// Ask the config service for a db connection string
func getDBConnection(db string) (string, error) {
	resp, err := http.DefaultClient.Get("http://config/config-service/dbconnection/" + db)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%v %v", resp.StatusCode, string(body))
	}
	return string(body), nil
}

func upgradeCmd(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("upgrade expected 3 arguments, but %v given", len(args))
	}
	logfile := args[0]
	db := args[1]
	sqlDir := args[2]
	return upgrade(logfile, db, sqlDir)
}

func upgrade(logfile, db, sqlDir string) error {
	logger := log.New(logfile, true)
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
		return fmt.Errorf("Error scanning %v: %v", sqlDir, err)
	}
	if len(sqlFiles) == 0 {
		return fmt.Errorf("No SQL files found in %v", sqlDir)
	}
	err = runMigrations(logger, db, sqlFiles)
	if err != nil {
		con, _ := parseDBConStr(db)
		logger.Errorf("%v: %v", con.dbname, err)
		return fmt.Errorf("%v: %v", con.dbname, err)
	}
	return nil
}

func upgradeAll(logfile string) error {
	//iterate over the folders in the migration root
	files, err := ioutil.ReadDir(migrationsRoot)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.IsDir() {
			conn, err := getDBConnection(f.Name())
			if err != nil {
				return err
			}
			migrationsDir := filepath.Join(migrationsRoot, f.Name())
			if err := upgrade(logfile, conn, migrationsDir); err != nil {
				return err
			}
		}
	}
	return nil
}


// This only has to run in docker. On Windows, migrations are run from the shell
// WARNING. There is no security check here. The implicit security model here is that
// since this service is not exposed to the router, it's not exposed to the outside
// world, so it has the same security model as the config service.
func serviceCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("service expected 1 arguments, but %v given", len(args))
	}
	port := args[0]
	logfile := "/var/log/imqs/migrator.log"
	logger := log.New(logfile, true)

	err := upgradeAll(logfile)
	if err != nil {
		logger.Errorf("Failed to upgrade DBs on startup: %v", err)
		return err
	}

	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, `{"Timestamp": %v}`, time.Now().UTC().Unix())
	})
	http.HandleFunc("/upgrade/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Must use a POST request", http.StatusBadRequest)
			return
		}
		dbName := r.URL.Path[9:]
		if !validDBNameRegex.MatchString(dbName) {
			http.Error(w, fmt.Sprintf("Invalid db name '%v'. Must be ASCII only", dbName), http.StatusBadRequest)
			return
		}
		dbCon, err := getDBConnection(dbName)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to fetch db connection for %v: %v", dbName, err), http.StatusBadRequest)
			return
		}
		migrationsDir := filepath.Join(migrationsRoot, dbName)
		if _, err := os.Stat(migrationsDir); err != nil {
			http.Error(w, fmt.Sprintf("No migrations found for database '%v'", dbName), http.StatusBadRequest)
			return
		}
		if err := upgrade(logfile, dbCon, migrationsDir); err != nil {
			http.Error(w, fmt.Sprintf("Upgrade of %v failed: %v", dbName, err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "OK")
	})
	http.HandleFunc("/schema/", func(w http.ResponseWriter, r *http.Request) {
		// Read a schema file
		filename := r.URL.Path[8:]
		if !validSchemaNameRegex.MatchString(filename) {
			http.Error(w, "Invalid request. Must be just the schema name, eg 'main', or 'mirror'", http.StatusBadRequest)
			return
		}
		filename = filepath.Join("/dbschema/schema", filename) + ".schema"
		http.ServeFile(w, r, filename)
	})

	addr := ":" + port
	logger.Infof("Listening on %v", addr)
	err = http.ListenAndServe(addr, nil)
	logger.Infof("Listener exited with %v", err)
	return err
}

func showHelp() {
	fmt.Printf("migrator [upgrade ... | service ...]\n")
	fmt.Printf("version 1.0.1\n")
	fmt.Printf(" upgrade <logfile> <db> <path to sql files>  Migrate a database up to the latest version available\n")
	fmt.Printf(" serve <port>                                Run as an HTTP service, listening on <port>\n")
}

func main() {
	if len(os.Args) < 3 {
		showHelp()
		os.Exit(1)
	}
	cmd := os.Args[1]
	if cmd == "upgrade" {
		if err := upgradeCmd(os.Args[2:]); err != nil {
			fmt.Printf("%v", err)
			os.Exit(1)
		}
	} else if cmd == "serve" {
		if err := serviceCmd(os.Args[2:]); err != nil {
			fmt.Printf("%v", err)
			os.Exit(1)
		}
	} else {
		fmt.Printf("Unknown command '%v'\n", cmd)
		showHelp()
		os.Exit(1)
	}
}
