package review

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectQualityCommands(t *testing.T) {
	tests := map[string]struct {
		setup func(dir string)
		want  QualityCommands
	}{
		"forge config highest priority": {
			setup: func(dir string) {
				writeFile(t, dir, ".forge/config.json", `{
					"quality": {
						"lint": ["golangci-lint run ./..."],
						"test": ["go test ./..."],
						"format": ["gofmt -l ."]
					}
				}`)
			},
			want: QualityCommands{
				Lint:   []string{"golangci-lint run ./..."},
				Test:   []string{"go test ./..."},
				Format: []string{"gofmt -l ."},
			},
		},
		"forge config partial — lint from config, test from go.mod": {
			setup: func(dir string) {
				writeFile(t, dir, ".forge/config.json", `{
					"quality": {
						"lint": ["golangci-lint run ./..."]
					}
				}`)
				writeFile(t, dir, "go.mod", "module greendale.edu/paintball\n\ngo 1.22\n")
			},
			want: QualityCommands{
				Lint: []string{"golangci-lint run ./..."},
				Test: []string{"go test ./..."},
			},
		},
		"forge config empty arrays — no fallthrough": {
			setup: func(dir string) {
				writeFile(t, dir, ".forge/config.json", `{
					"quality": {
						"lint": [],
						"test": [],
						"format": []
					}
				}`)
				writeFile(t, dir, "go.mod", "module greendale.edu/paintball\n\ngo 1.22\n")
			},
			want: QualityCommands{
				Lint:   []string{},
				Test:   []string{},
				Format: []string{},
			},
		},
		"justfile targets": {
			setup: func(dir string) {
				writeFile(t, dir, "justfile", `
test:
    go test ./...

lint:
    golangci-lint run ./...

fmt:
    gofmt -w .

build:
    go build ./...
`)
			},
			want: QualityCommands{
				Lint:   []string{"golangci-lint run ./..."},
				Test:   []string{"go test ./..."},
				Format: []string{"gofmt -w ."},
			},
		},
		"Makefile targets": {
			setup: func(dir string) {
				writeFile(t, dir, "Makefile", `
.PHONY: test lint format

test:
	go test ./...

lint:
	golangci-lint run ./...

format:
	gofmt -w .
`)
			},
			want: QualityCommands{
				Lint:   []string{"golangci-lint run ./..."},
				Test:   []string{"go test ./..."},
				Format: []string{"gofmt -w ."},
			},
		},
		"package.json scripts": {
			setup: func(dir string) {
				writeFile(t, dir, "package.json", `{
					"scripts": {
						"lint": "eslint .",
						"test": "jest",
						"format": "prettier --check ."
					}
				}`)
			},
			want: QualityCommands{
				Lint:   []string{"eslint ."},
				Test:   []string{"jest"},
				Format: []string{"prettier --check ."},
			},
		},
		"go.mod infers go test and go vet": {
			setup: func(dir string) {
				writeFile(t, dir, "go.mod", "module greendale.edu/paintball\n\ngo 1.22\n")
			},
			want: QualityCommands{
				Test: []string{"go test ./..."},
			},
		},
		"no recognizable build system": {
			setup: func(dir string) {
				// empty project
			},
			want: QualityCommands{},
		},
		"justfile overrides go.mod for same category": {
			setup: func(dir string) {
				writeFile(t, dir, "justfile", `
test:
    just-test-runner ./...
`)
				writeFile(t, dir, "go.mod", "module greendale.edu/paintball\n\ngo 1.22\n")
			},
			want: QualityCommands{
				Test: []string{"just-test-runner ./..."},
			},
		},
		"multiple targets in justfile": {
			setup: func(dir string) {
				writeFile(t, dir, "justfile", `
lint:
    golangci-lint run ./...

check:
    go vet ./...
`)
			},
			want: QualityCommands{
				Lint: []string{"golangci-lint run ./...", "go vet ./..."},
			},
		},
		"github workflows": {
			setup: func(dir string) {
				writeFile(t, dir, ".github/workflows/ci.yml", `
name: CI
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: golangci-lint run ./...
`)
			},
			want: QualityCommands{
				Lint: []string{"golangci-lint run ./..."},
				Test: []string{"go test ./..."},
			},
		},
		"github workflow skips template variables": {
			setup: func(dir string) {
				writeFile(t, dir, ".github/workflows/ci.yml", `
name: CI
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    steps:
      - run: go test ./...
      - run: echo ${{ matrix.os }}
`)
			},
			want: QualityCommands{
				Test: []string{"go test ./..."},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			dir := t.TempDir()
			tc.setup(dir)

			got := DetectQualityCommands(dir)
			r.Equal(tc.want, got)
		})
	}
}

func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	abs := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
}
