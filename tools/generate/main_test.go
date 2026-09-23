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
	for _, path := range []string{"go.mod", "go.sum", "api/openapi.yaml", "api/oapi-codegen.yaml", "api/oapi-client.yaml", "sqlc.yaml"} {
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
	for _, path := range []string{"client/client.gen.go", "internal/store/db/models.go", "internal/store/db/store.sql.go"} {
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
	const handwritten = "client/doc.go"
	if err := os.WriteFile(handwritten, []byte("// Package client calls Orpheus.\npackage client\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, obsolete := range []string{"internal/store/db/obsolete.sql.go", "internal/api/obsolete.go", "client/obsolete.gen.go"} {
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
		if _, err := os.Stat(handwritten); err != nil {
			t.Fatal("generation removed handwritten client code")
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

func TestClientOnlyGeneration(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("api", 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"go.mod", "go.sum", "api/openapi.yaml", "api/oapi-client.yaml"} {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	// There are no SQL inputs or server generator configuration in this fixture.
	if err := generateTargets(true, true); err == nil {
		t.Fatal("missing client output accepted")
	}
	if _, err := os.Stat("client"); !os.IsNotExist(err) {
		t.Fatal("check created client directory")
	}
	if err := generateTargets(false, true); err != nil {
		t.Fatal(err)
	}
	if err := generateTargets(true, true); err != nil {
		t.Fatal(err)
	}
	path := "client/client.gen.go"
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package client\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := generateTargets(true, true); err == nil {
		t.Fatal("stale client accepted")
	}
	if err := generateTargets(false, true); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("client generation not reproducible: %v", err)
	}
	for _, path := range []string{"internal/api", "internal/store/db"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("client generation touched %s", path)
		}
	}
}
