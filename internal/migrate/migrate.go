// Package migrate applies the Goose SQL files shipped with the application.
package migrate

import (
	"context"
	"database/sql"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func Provider(db *sql.DB, directory string) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectPostgres, db, os.DirFS(directory))
}
func Run(ctx context.Context, url, command, directory string) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	p, err := Provider(db, directory)
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
