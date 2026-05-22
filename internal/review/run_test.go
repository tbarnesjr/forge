package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStripANSI(t *testing.T) {
	tests := map[string]struct {
		input string
		want  string
	}{
		"no ANSI": {
			input: "foo.go:42: error here",
			want:  "foo.go:42: error here",
		},
		"color codes": {
			input: "\x1b[31mfoo.go:42: error\x1b[0m",
			want:  "foo.go:42: error",
		},
		"bold and reset": {
			input: "\x1b[1m\x1b[31merror\x1b[0m: foo.go:10: problem",
			want:  "error: foo.go:10: problem",
		},
		"empty": {
			input: "",
			want:  "",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			r.Equal(tc.want, stripANSI(tc.input))
		})
	}
}

func TestParseCommandOutput(t *testing.T) {
	tests := map[string]struct {
		output   string
		category string
		want     []Finding
	}{
		"file:line format — lint": {
			output:   "foo.go:42: exported function Foo should have comment\nbar.go:10: shadowed variable x",
			category: "lint",
			want: []Finding{
				{Reviewer: "project-quality", Provider: "local", Severity: SeverityWarning, File: "foo.go", StartLine: 42, Description: "exported function Foo should have comment"},
				{Reviewer: "project-quality", Provider: "local", Severity: SeverityWarning, File: "bar.go", StartLine: 10, Description: "shadowed variable x"},
			},
		},
		"file:line:col format": {
			output:   "src/main.ts:5:12: Unexpected any",
			category: "lint",
			want: []Finding{
				{Reviewer: "project-quality", Provider: "local", Severity: SeverityWarning, File: "src/main.ts", StartLine: 5, Description: "Unexpected any"},
			},
		},
		"no parseable locations — test failure": {
			output:   "FAIL: TestTroyBarnes\n  Expected true, got false",
			category: "test",
			want: []Finding{
				{Reviewer: "project-quality", Provider: "local", Severity: SeverityCritical, File: "", Description: "FAIL: TestTroyBarnes\n  Expected true, got false"},
			},
		},
		"format violations": {
			output:   "study_room_f.go\npaintball.go",
			category: "format",
			want: []Finding{
				{Reviewer: "project-quality", Provider: "local", Severity: SeveritySuggestion, File: "", Description: "study_room_f.go\npaintball.go"},
			},
		},
		"ANSI stripped before parsing": {
			output:   "\x1b[31mfoo.go:42: error here\x1b[0m",
			category: "lint",
			want: []Finding{
				{Reviewer: "project-quality", Provider: "local", Severity: SeverityWarning, File: "foo.go", StartLine: 42, Description: "error here"},
			},
		},
		"empty output": {
			output:   "",
			category: "lint",
			want:     nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			got := parseCommandOutput(tc.output, tc.category)
			r.Equal(tc.want, got)
		})
	}
}

func TestSeverityForCategory(t *testing.T) {
	r := require.New(t)
	r.Equal(SeverityCritical, severityForCategory("test"))
	r.Equal(SeverityWarning, severityForCategory("lint"))
	r.Equal(SeveritySuggestion, severityForCategory("format"))
	r.Equal(SeverityWarning, severityForCategory("unknown"))
}

func TestCommandNeedsSecrets(t *testing.T) {
	tests := map[string]struct {
		cmd  string
		want bool
	}{
		"clean command":       {cmd: "go test ./...", want: false},
		"docker push":         {cmd: "docker push myimg", want: true},
		"npm publish":         {cmd: "npm publish --tag latest", want: true},
		"secret var":          {cmd: "echo $SECRET_KEY", want: true},
		"aws var":             {cmd: "aws s3 sync $AWS_BUCKET s3://x", want: true},
		"docker var":          {cmd: "docker login $DOCKER_USERNAME", want: true},
		"curl external":       {cmd: "curl https://api.external.com/deploy", want: true},
		"curl localhost fine": {cmd: "curl http://localhost:8080/health", want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			r.Equal(tc.want, commandNeedsSecrets(tc.cmd))
		})
	}
}

func TestProjectQualityReviewer_Run(t *testing.T) {
	tests := map[string]struct {
		setup func(dir string)
		check func(t *testing.T, findings []Finding)
	}{
		"passing command — no findings": {
			setup: func(dir string) {
				writeFile(t, dir, ".forge/config.json", `{
					"quality": {
						"test": ["true"]
					}
				}`)
			},
			check: func(t *testing.T, findings []Finding) {
				r := require.New(t)
				r.Empty(findings)
			},
		},
		"failing command — produces findings": {
			setup: func(dir string) {
				writeFile(t, dir, ".forge/config.json", `{
					"quality": {
						"test": ["false"]
					}
				}`)
			},
			check: func(t *testing.T, findings []Finding) {
				r := require.New(t)
				r.NotEmpty(findings)
				r.Equal("project-quality", findings[0].Reviewer)
				r.Equal("local", findings[0].Provider)
				r.Equal(SeverityCritical, findings[0].Severity)
			},
		},
		"command with parseable output": {
			setup: func(dir string) {
				// Create a script that outputs file:line format
				script := filepath.Join(dir, "lint.sh")
				writeFile(t, dir, "lint.sh", "#!/bin/sh\necho 'foo.go:42: Troy Barnes would not approve'\nexit 1\n")
				os.Chmod(script, 0o755)
				writeFile(t, dir, ".forge/config.json", `{
					"quality": {
						"lint": ["./lint.sh"]
					}
				}`)
			},
			check: func(t *testing.T, findings []Finding) {
				r := require.New(t)
				r.Len(findings, 1)
				r.Equal("foo.go", findings[0].File)
				r.Equal(42, findings[0].StartLine)
				r.Contains(findings[0].Description, "Troy Barnes")
			},
		},
		"no quality commands detected": {
			setup: func(dir string) {
				// empty project
			},
			check: func(t *testing.T, findings []Finding) {
				r := require.New(t)
				r.Empty(findings)
			},
		},
		"command needs secrets — skipped": {
			setup: func(dir string) {
				writeFile(t, dir, ".forge/config.json", `{
					"quality": {
						"test": ["docker push myimage"]
					}
				}`)
			},
			check: func(t *testing.T, findings []Finding) {
				r := require.New(t)
				r.Len(findings, 1)
				r.Equal(SeveritySuggestion, findings[0].Severity)
				r.Contains(findings[0].Description, "Skipped")
			},
		},
		"command timeout": {
			setup: func(dir string) {
				writeFile(t, dir, ".forge/config.json", `{
					"quality": {
						"test": ["sleep 60"],
						"timeout": "100ms"
					}
				}`)
			},
			check: func(t *testing.T, findings []Finding) {
				r := require.New(t)
				r.Len(findings, 1)
				r.Equal(SeverityWarning, findings[0].Severity)
				r.Contains(findings[0].Description, "timed out")
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(dir)

			reviewer := ProjectQualityReviewer{}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			findings := reviewer.Run(ctx, dir)
			tc.check(t, findings)
		})
	}
}

func TestProjectQualityReviewerInterface(t *testing.T) {
	r := require.New(t)

	var rev DeterministicReviewer = ProjectQualityReviewer{}
	r.Equal("project-quality", rev.Name())
	r.Equal("", rev.SystemPrompt())
}
