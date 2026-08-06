package trajectorysql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readExportSQL(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "case-studies", "isucon14-config", "export-trajectory.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestISUCON14TrajectoryExportAvoidsReservedRowNumberAlias(t *testing.T) {
	script := strings.ToLower(readExportSQL(t))
	for _, reservedUse := range []string{"as row_number", "where row_number"} {
		if strings.Contains(script, reservedUse) {
			t.Errorf("export SQL contains reserved alias use %q", reservedUse)
		}
	}
	if !strings.Contains(script, "as point_rank") || !strings.Contains(script, "where point_rank = 1") {
		t.Error("export SQL does not define and consume the point_rank alias")
	}
}
