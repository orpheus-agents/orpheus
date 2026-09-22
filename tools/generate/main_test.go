package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerationChecksContentWithoutMutatingFiles(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	for _, dir := range []string{"api", "internal/api"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"migrations", "internal/store/queries"} {
		if err := os.CopyFS(dir, os.DirFS(filepath.Join(root, dir))); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"go.mod", "go.sum", "api/openapi.yaml", "api/oapi-codegen.yaml", "sqlc.yaml"} {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := generate(false); err != nil {
		t.Fatal(err)
	}
	if err := generate(true); err != nil {
		t.Fatal(err)
	}
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	original := read("internal/api/generated.go")
	stale := append(bytes.Clone(original), []byte("\n// stale output\n")...)
	if err := os.WriteFile("internal/api/generated.go", stale, 0644); err != nil {
		t.Fatal(err)
	}
	if err := generate(true); err == nil {
		t.Fatal("stale untracked output accepted")
	}
	if !bytes.Equal(read("internal/api/generated.go"), stale) {
		t.Fatal("check changed existing output")
	}
	if err := os.WriteFile("internal/api/generated.go", original, 0644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"internal/store/db/models.go", "internal/store/db/store.sql.go"} {
		originalSQL := read(path)
		staleSQL := append(bytes.Clone(originalSQL), []byte("\n// stale output\n")...)
		if err := os.WriteFile(path, staleSQL, 0644); err != nil {
			t.Fatal(err)
		}
		if err := generate(true); err == nil {
			t.Fatalf("stale SQL output accepted: %s", path)
		}
		if !bytes.Equal(read(path), staleSQL) {
			t.Fatalf("check changed SQL output: %s", path)
		}
		if err := os.WriteFile(path, originalSQL, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, obsolete := range []string{"internal/store/db/obsolete.sql.go", "internal/api/obsolete.go"} {
		if err := os.WriteFile(obsolete, []byte("package db\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := generate(true); err == nil {
			t.Fatal("obsolete generated output accepted")
		}
		if _, err := os.Stat(obsolete); err != nil {
			t.Fatal("check removed obsolete output")
		}
		if err := generate(false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
			t.Fatal("generation retained obsolete output")
		}
	}
	queryPath := "internal/store/queries/store.sql"
	queries := read(queryPath)
	if err := os.WriteFile(queryPath, []byte("-- name: Invalid :one\nSELECT missing_column FROM sessions;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	model := read("internal/store/db/models.go")
	if err := generate(false); err == nil {
		t.Fatal("invalid SQL accepted")
	}
	if !bytes.Equal(read("internal/store/db/models.go"), model) || !bytes.Equal(read("internal/api/generated.go"), original) {
		t.Fatal("failed SQL generation changed output")
	}
	if err := os.WriteFile(queryPath, queries, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("internal/api/generated.go", stale, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("api/openapi.yaml", []byte("invalid: ["), 0644); err != nil {
		t.Fatal(err)
	}
	if err := generate(false); err == nil {
		t.Fatal("generator failure hidden")
	}
	if !bytes.Equal(read("internal/api/generated.go"), stale) {
		t.Fatal("failed generation changed output")
	}
}
