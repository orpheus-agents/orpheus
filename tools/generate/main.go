// Command generate derives transport code, the embedded specification, and SQL queries.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

func main() {
	check := flag.Bool("check", false, "compare generated content without changing files")
	flag.Parse()
	if err := generate(*check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func generate(check bool) error {
	dir, err := os.MkdirTemp("", "orpheus-generate-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	generated := filepath.Join(dir, "generated.go")
	command := exec.Command("go", "tool", "oapi-codegen", "-config", "api/oapi-codegen.yaml", "-o", generated, "api/openapi.yaml")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return err
	}
	// Run sqlc against copied inputs so check mode never rewrites the checkout.
	for _, path := range []string{"migrations", "internal/store/queries"} {
		if err := os.CopyFS(filepath.Join(dir, path), os.DirFS(path)); err != nil {
			return err
		}
	}
	config, err := os.ReadFile("sqlc.yaml")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "sqlc.yaml"), config, 0644); err != nil {
		return err
	}
	command = exec.Command("sqlc", "generate")
	command.Dir = dir
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return err
	}
	const sqlPath = "internal/store/db"
	entries, err := os.ReadDir(filepath.Join(dir, sqlPath))
	if err != nil {
		return err
	}
	outputs := map[string][]byte{}
	for _, entry := range entries {
		path := filepath.Join(sqlPath, entry.Name())
		code, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return err
		}
		outputs[path] = code
	}
	code, err := os.ReadFile(generated)
	if err != nil {
		return err
	}
	outputs["internal/api/generated.go"] = code
	// Both directories contain only generated files. Obsolete files must also be
	// rejected when they are untracked by Git.
	var obsolete []string
	for _, directory := range []string{sqlPath, "internal/api"} {
		actualEntries, err := os.ReadDir(directory)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, entry := range actualEntries {
			path := filepath.Join(directory, entry.Name())
			if _, ok := outputs[path]; !ok {
				obsolete = append(obsolete, path)
			}
		}
	}
	paths := make([]string, 0, len(outputs))
	for path := range outputs {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	if check {
		if len(obsolete) > 0 {
			return fmt.Errorf("obsolete generated files: %v; run make generate", obsolete)
		}
		for _, path := range paths {
			actual, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(actual, outputs[path]) {
				return fmt.Errorf("%s is stale; run make generate", path)
			}
		}
		return nil
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, outputs[path], 0644); err != nil {
			return err
		}
	}
	for _, path := range obsolete {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
