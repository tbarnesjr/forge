package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	tests := map[string]struct {
		projectJSON string
		want        ForgeConfig
	}{
		"no config files": {
			want: ForgeConfig{},
		},
		"project config with specsDir": {
			projectJSON: `{"specsDir": "specs/greendale"}`,
			want:        ForgeConfig{SpecsDir: "specs/greendale"},
		},
		"empty project config": {
			projectJSON: `{}`,
			want:        ForgeConfig{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			dir := t.TempDir()

			if tc.projectJSON != "" {
				forgeDir := filepath.Join(dir, ".forge")
				r.NoError(os.MkdirAll(forgeDir, 0o755))
				r.NoError(os.WriteFile(filepath.Join(forgeDir, "config.json"), []byte(tc.projectJSON), 0o644))
			}

			got, err := Load(dir)
			r.NoError(err)
			r.Equal(tc.want, got)
		})
	}
}

func TestLoad_withQuality(t *testing.T) {
	tests := map[string]struct {
		projectJSON string
		want        ForgeConfig
	}{
		"quality with all fields": {
			projectJSON: `{"quality":{"lint":["golangci-lint run ./..."],"test":["go test ./..."],"format":["gofmt -l ."],"timeout":"3m"}}`,
			want: ForgeConfig{
				Quality: &QualityConfig{
					Lint:    []string{"golangci-lint run ./..."},
					Test:    []string{"go test ./..."},
					Format:  []string{"gofmt -l ."},
					Timeout: "3m",
				},
			},
		},
		"quality with empty arrays": {
			projectJSON: `{"quality":{"lint":[],"test":[],"format":[]}}`,
			want: ForgeConfig{
				Quality: &QualityConfig{
					Lint:   []string{},
					Test:   []string{},
					Format: []string{},
				},
			},
		},
		"quality with only lint": {
			projectJSON: `{"quality":{"lint":["eslint ."]}}`,
			want: ForgeConfig{
				Quality: &QualityConfig{
					Lint: []string{"eslint ."},
				},
			},
		},
		"specsDir and quality combined": {
			projectJSON: `{"specsDir":"specs/greendale","quality":{"test":["go test ./..."]}}`,
			want: ForgeConfig{
				SpecsDir: "specs/greendale",
				Quality: &QualityConfig{
					Test: []string{"go test ./..."},
				},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			dir := t.TempDir()

			forgeDir := filepath.Join(dir, ".forge")
			r.NoError(os.MkdirAll(forgeDir, 0o755))
			r.NoError(os.WriteFile(filepath.Join(forgeDir, "config.json"), []byte(tc.projectJSON), 0o644))

			got, err := Load(dir)
			r.NoError(err)
			r.Equal(tc.want, got)
		})
	}
}

func TestLoad_invalidJSON(t *testing.T) {
	r := require.New(t)
	dir := t.TempDir()

	forgeDir := filepath.Join(dir, ".forge")
	r.NoError(os.MkdirAll(forgeDir, 0o755))
	r.NoError(os.WriteFile(filepath.Join(forgeDir, "config.json"), []byte(`{not json`), 0o644))

	_, err := Load(dir)
	r.Error(err)
	r.Contains(err.Error(), "parse")
}
