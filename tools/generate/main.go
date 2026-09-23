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
	"strings"
)

func main() {
	check := flag.Bool("check", false, "compare generated content without changing files")
	clientOnly := flag.Bool("client", false, "generate only the public Go client")
	flag.Parse()
	if err := generateTargets(*check, *clientOnly); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func generate(check bool) error {
	return generateTargets(check, false)
}

func generateTargets(check, clientOnly bool) error {
	dir, err := os.MkdirTemp("", "orpheus-generate-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	outputs := map[string][]byte{}
	for _, target := range []struct{ config, output string }{
		{"api/oapi-codegen.yaml", "internal/api/generated.go"},
		{"api/oapi-client.yaml", "client/client.gen.go"},
	} {
		if clientOnly && target.output != "client/client.gen.go" {
			continue
		}
		generated := filepath.Join(dir, filepath.Base(target.output))
		command := exec.Command("go", "tool", "oapi-codegen", "-config", target.config, "-o", generated, "api/openapi.yaml")
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			return err
		}
		code, err := os.ReadFile(generated)
		if err != nil {
			return err
		}
		outputs[target.output] = code
	}
	const sqlPath = "internal/store/db"
	if !clientOnly {
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
		command := exec.Command("sqlc", "generate")
		command.Dir = dir
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			return err
		}
		entries, err := os.ReadDir(filepath.Join(dir, sqlPath))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			path := filepath.Join(sqlPath, entry.Name())
			code, err := os.ReadFile(filepath.Join(dir, path))
			if err != nil {
				return err
			}
			outputs[path] = code
		}
	}
	// The internal directories contain only generated files. The public client
	// also has handwritten code; only *.gen.go belongs to the generator there.
	var obsolete []string
	directories := []string{"client"}
	if !clientOnly {
		directories = append(directories, sqlPath, "internal/api")
	}
	for _, directory := range directories {
		actualEntries, err := os.ReadDir(directory)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, entry := range actualEntries {
			if directory == "client" && !strings.HasSuffix(entry.Name(), ".gen.go") {
				continue
			}
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
