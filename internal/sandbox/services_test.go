package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolve(t *testing.T) {
	pgTuning := []string{"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"}
	myTuning := []string{"--innodb-flush-log-at-trx-commit=0", "--sync-binlog=0", "--skip-log-bin"}
	tests := []struct {
		fixture string
		source  string
		want    []Service
	}{
		{
			fixture: "file-postgres",
			source:  SourceFile,
			want: []Service{{
				Name: "postgres", Image: "postgres:16", Registry: "docker.io",
				Env:     map[string]string{"POSTGRES_USER": "postgres", "POSTGRES_PASSWORD": "postgres", "POSTGRES_DB": "app_test"},
				Ports:   []string{"5432:5432"},
				Command: pgTuning, Tuned: true,
				Health:   &HealthCheck{Cmd: "pg_isready -U postgres", Interval: 2 * time.Second, Timeout: 5 * time.Second, Retries: 30},
				Required: true, From: ".mesh/services.yml",
			}},
		},
		{
			fixture: "workflow-redis",
			source:  SourceWorkflow,
			want: []Service{{
				Name: "redis", Image: "redis:7-alpine", Registry: "docker.io",
				Env:    map[string]string{},
				Ports:  []string{"6379:6379"},
				Health: &HealthCheck{Cmd: "redis-cli ping", Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5},
				From:   ".github/workflows/ci.yml",
			}},
		},
		{
			fixture: "workflow-mysql",
			source:  SourceWorkflow,
			want: []Service{{
				Name: "mysql", Image: "mysql:8.0", Registry: "docker.io",
				Env:     map[string]string{"MYSQL_ROOT_PASSWORD": "root", "MYSQL_DATABASE": "app_test"},
				Ports:   []string{"3306:3306"},
				Command: myTuning, Tuned: true,
				Health: &HealthCheck{Cmd: "mysqladmin ping", Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 3},
				From:   ".github/workflows/test.yml",
				Notes: []string{
					`env MYSQL_PASSWORD "${{ secrets.MYSQL_PASSWORD }}": expression dropped`,
					"option --cpus ignored",
				},
			}},
		},
		{
			// services.yml wins: the workflow's redis is not read, and a
			// declared command means no tuning.
			fixture: "both",
			source:  SourceFile,
			want: []Service{{
				Name: "postgres", Image: "postgres:17", Registry: "docker.io",
				Env:     map[string]string{"POSTGRES_PASSWORD": "postgres"},
				Ports:   []string{"5432:5432"},
				Command: []string{"-c", "max_connections=200"},
				From:    ".mesh/services.yml",
			}},
		},
		{fixture: "neither", source: SourceNone},
		{fixture: "non-test", source: SourceNone},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got, err := Resolve("testdata/"+tt.fixture, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.Source != tt.source {
				t.Errorf("source = %q, want %q", got.Source, tt.source)
			}
			if !reflect.DeepEqual(got.Services, tt.want) {
				t.Errorf("services =\n%+v\nwant\n%+v", got.Services, tt.want)
			}
		})
	}
}

func TestResolveRegistries(t *testing.T) {
	got, err := Resolve("testdata/registries", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{ // name → registry; evil is the only refused
		"hub": "docker.io", "hub-explicit": "docker.io", "ghcr": "ghcr.io",
		"gar": "us-docker.pkg.dev", "quay": "quay.io", "evil": "evil.example.com",
	}
	if len(got.Services) != len(want) {
		t.Fatalf("got %d services, want %d", len(got.Services), len(want))
	}
	for _, s := range got.Services {
		if s.Registry != want[s.Name] {
			t.Errorf("%s: registry %q, want %q", s.Name, s.Registry, want[s.Name])
		}
		if s.Refused != (s.Name == "evil") {
			t.Errorf("%s: refused = %v", s.Name, s.Refused)
		}
	}
}

func TestResolveDisallowed(t *testing.T) {
	got, err := Resolve("testdata/disallowed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Services) != 1 {
		t.Fatalf("services = %+v, want the refused one returned", got.Services)
	}
	s := got.Services[0]
	if !s.Refused || s.Image != "evil.example.com/pg:16" {
		t.Fatalf("service = %+v, want refused", s)
	}
	want := "image evil.example.com/pg:16 refused: registry evil.example.com is not allowed (allowed: docker.io, ghcr.io, *.pkg.dev, quay.io)"
	if !reflect.DeepEqual(s.Notes, []string{want}) {
		t.Errorf("notes = %q, want %q", s.Notes, want)
	}

	// An allow-list that names it lets it through.
	got, _ = Resolve("testdata/disallowed", []string{"evil.example.com"})
	if got.Services[0].Refused {
		t.Errorf("refused with evil.example.com allowed")
	}
}

func TestResolveBadFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".mesh/services.yml", "services:\n  pg:\n    ports: [5432]\n")
	if _, err := Resolve(dir, nil); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Errorf("err = %v, want no image", err)
	}
}

func TestResolveWorkflowMerge(t *testing.T) {
	dir := t.TempDir()
	// a.yml comes first, so its postgres wins over b.yml's; an image that
	// cannot be resolved is skipped with a note; tuning: false is honoured.
	write(t, dir, ".github/workflows/a.yml", `
jobs:
  e2e-tests:
    services:
      postgres:
        image: postgres:15
        tuning: false
      broken:
        image: ${{ vars.IMAGE }}
    steps: [{run: ./e2e.sh}]
`)
	write(t, dir, ".github/workflows/b.yml", `
jobs:
  check:
    services:
      postgres: {image: postgres:16}
      redis: {image: redis:7}
    steps: [{run: "pytest -q"}]
`)
	got, err := Resolve(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got.Services {
		names = append(names, s.Name+"="+s.Image)
		if s.Name == "postgres" && s.Tuned {
			t.Errorf("postgres tuned despite tuning: false")
		}
	}
	if want := []string{"postgres=postgres:15", "redis=redis:7"}; !reflect.DeepEqual(names, want) {
		t.Errorf("services = %v, want %v", names, want)
	}
	if len(got.Notes) != 1 || !strings.Contains(got.Notes[0], "service broken skipped") {
		t.Errorf("notes = %q", got.Notes)
	}
}

func TestSplitArgs(t *testing.T) {
	got := splitArgs(`--health-cmd "pg_isready -U postgres" --x='a b' c\ d`)
	want := []string{"--health-cmd", "pg_isready -U postgres", "--x=a b", "c d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
