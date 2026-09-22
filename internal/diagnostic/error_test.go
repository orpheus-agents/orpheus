package diagnostic

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDescribePreservesTypeAndSQLStateWithoutData(t *testing.T) {
	err := fmt.Errorf("password=secret: %w", &pgconn.PgError{Code: "23505", Message: "secret", Detail: "private SQL", TableName: "private_table"})
	got := Describe(err)
	if !strings.Contains(got, "pgconn.PgError SQLSTATE=23505") || strings.Contains(got, "secret") || strings.Contains(got, "private") {
		t.Fatal(got)
	}
}
