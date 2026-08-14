package sqlstats

import (
	"database/sql"
	"testing"
)

func init() {
	for _, name := range []string{"compat-mysql", "compat-mariadb", "compat-pgx", "compat-sqlite"} {
		sql.Register(name, fakeDriver{})
	}
}

func TestDatabaseSQLDriverFamiliesUseOneInstrumentationContract(t *testing.T) {
	for _, driverName := range []string{"compat-mysql", "compat-mariadb", "compat-pgx", "compat-sqlite"} {
		t.Run(driverName, func(t *testing.T) {
			if err := Register(driverName); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open(driverName+DriverSuffix, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Errorf("close %s: %v", driverName, err)
				}
			}()
			if _, err := db.Exec("SELECT ?", driverName); err != nil {
				t.Fatal(err)
			}
			if len(Default.Snapshot()) == 0 {
				t.Fatalf("%s observation was not recorded", driverName)
			}
			Default.Reset()
		})
	}
}
