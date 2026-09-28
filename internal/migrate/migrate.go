// Package migrate applies the Goose SQL files shipped with the application.
package migrate

import (
	"context"
	"database/sql"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

func Provider(db *sql.DB, directory string, opts ...goose.ProviderOption) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectPostgres, db, os.DirFS(directory), opts...)
}
func Run(ctx context.Context, url, command, directory string) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	p, err := Provider(db, directory, goose.WithSessionLocker(sessionLocker{locker}))
	if err != nil {
		return err
	}
	switch command {
	case "down":
		_, err = p.Down(ctx)
	case "reset":
		_, err = p.DownTo(ctx, 0)
	default:
		_, err = p.Up(ctx)
	}
	return err
}
