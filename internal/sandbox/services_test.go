package sandbox

import (
	"context"
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
				Env: map[string]string{
					"POSTGRES_USER": "postgres", "POSTGRES_PASSWORD": "postgres", "POSTGRES_DB": "app_test",
					"PGDATA": PostgresDefaultPGDATA, "POSTGRES_INITDB_ARGS": PostgresDefaultInitdbArgs,
				},
				Ports:   []string{"5432:5432"},
				Command: pgTuning, Tuned: true,
				Health:   &HealthCheck{Cmd: "pg_isready -U postgres", Interval: 2 * time.Second, Timeout: 5 * time.Second, Retries: 30},
				Required: true, From: ".mesh/services.yml",
				Tmpfs: []Tmpfs{{Path: "/tmp"}},
				Notes: []string{"defaulted PGDATA, POSTGRES_INITDB_ARGS so the data directory is writable under keep-id", "data on a tmpfs at /tmp (RAM, not the container overlay): tmpfs: false turns it off"},
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
				Env: map[string]string{
					"POSTGRES_PASSWORD": "postgres",
					"PGDATA":            PostgresDefaultPGDATA, "POSTGRES_INITDB_ARGS": PostgresDefaultInitdbArgs,
				},
				Ports:   []string{"5432:5432"},
				Command: []string{"-c", "max_connections=200"},
				From:    ".mesh/services.yml",
				Notes:   []string{"defaulted PGDATA, POSTGRES_INITDB_ARGS so the data directory is writable under keep-id"},
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

func TestWorkflowPostgresServiceDefaultsPGDATA(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".github/workflows/test.yml", `
jobs:
  test:
    services:
      postgres:
        image: postgres:16
        env:
          POSTGRES_PASSWORD: postgres
    steps: [{run: "go test ./..."}]
`)
	got, err := Resolve(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceWorkflow {
		t.Errorf("source = %q, want %q", got.Source, SourceWorkflow)
	}
	if len(got.Services) != 1 {
		t.Fatalf("services = %+v, want one", got.Services)
	}
	s := got.Services[0]
	if s.Env["PGDATA"] != PostgresDefaultPGDATA {
		t.Errorf("PGDATA = %q, want %q", s.Env["PGDATA"], PostgresDefaultPGDATA)
	}
	if s.Env["POSTGRES_INITDB_ARGS"] != PostgresDefaultInitdbArgs {
		t.Errorf("POSTGRES_INITDB_ARGS = %q, want %q", s.Env["POSTGRES_INITDB_ARGS"], PostgresDefaultInitdbArgs)
	}
	if s.Env["POSTGRES_PASSWORD"] != "postgres" {
		t.Errorf("POSTGRES_PASSWORD = %q, want kept", s.Env["POSTGRES_PASSWORD"])
	}
}

func TestPostgresServiceDefaultsPGDATA(t *testing.T) {
	images := []string{
		"postgres:17-alpine",
		"postgres:16",
		"docker.io/library/postgres:17-alpine",
		"docker.io/library/postgres:16",
	}
	for _, image := range images {
		t.Run(image, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, ".mesh/services.yml", "services:\n  pg:\n    image: "+image+"\n")
			got, err := Resolve(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Services) != 1 {
				t.Fatalf("services = %+v, want one", got.Services)
			}
			s := got.Services[0]
			if s.Env["PGDATA"] != PostgresDefaultPGDATA {
				t.Errorf("PGDATA = %q, want %q", s.Env["PGDATA"], PostgresDefaultPGDATA)
			}
			if s.Env["POSTGRES_INITDB_ARGS"] != PostgresDefaultInitdbArgs {
				t.Errorf("POSTGRES_INITDB_ARGS = %q, want %q", s.Env["POSTGRES_INITDB_ARGS"], PostgresDefaultInitdbArgs)
			}
		})
	}
}

func TestPostgresServiceKeepsItsOwnPGDATA(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".mesh/services.yml", `
services:
  pg-own-pgdata:
    image: postgres:16
    env:
      PGDATA: /custom/data
  pg-own-initdb-args:
    image: postgres:16
    env:
      POSTGRES_INITDB_ARGS: --foo
`)
	got, err := Resolve(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Services) != 2 {
		t.Fatalf("services = %+v, want two", got.Services)
	}
	byName := map[string]Service{}
	for _, s := range got.Services {
		byName[s.Name] = s
	}
	pgdata := byName["pg-own-pgdata"]
	if pgdata.Env["PGDATA"] != "/custom/data" {
		t.Errorf("PGDATA = %q, want own value kept", pgdata.Env["PGDATA"])
	}
	if pgdata.Env["POSTGRES_INITDB_ARGS"] != PostgresDefaultInitdbArgs {
		t.Errorf("POSTGRES_INITDB_ARGS = %q, want the default", pgdata.Env["POSTGRES_INITDB_ARGS"])
	}
	initdbArgs := byName["pg-own-initdb-args"]
	if initdbArgs.Env["POSTGRES_INITDB_ARGS"] != "--foo" {
		t.Errorf("POSTGRES_INITDB_ARGS = %q, want own value kept", initdbArgs.Env["POSTGRES_INITDB_ARGS"])
	}
	if initdbArgs.Env["PGDATA"] != PostgresDefaultPGDATA {
		t.Errorf("PGDATA = %q, want the default", initdbArgs.Env["PGDATA"])
	}
}

func TestNonPostgresServiceGetsNoPGDATA(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".mesh/services.yml", `
services:
  redis:
    image: redis:7
  mysql:
    image: mysql:8
`)
	got, err := Resolve(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got.Services {
		if _, ok := s.Env["PGDATA"]; ok {
			t.Errorf("%s: env = %+v, want no PGDATA", s.Name, s.Env)
		}
		if _, ok := s.Env["POSTGRES_INITDB_ARGS"]; ok {
			t.Errorf("%s: env = %+v, want no POSTGRES_INITDB_ARGS", s.Name, s.Env)
		}
		if len(s.Env) != 0 {
			t.Errorf("%s: env = %+v, want empty", s.Name, s.Env)
		}
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

// resolveFile resolves a .mesh/services.yml of body.
func resolveFile(t *testing.T, body string) ([]Service, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".mesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ServicesFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(dir, nil)
	return got.Services, err
}

// A Postgres the node tunes keeps its data on a tmpfs at PGDATA's parent
// (dl360's syncfs stall, 2026-10-28), saying so; `tmpfs: false`,
// `tuning: false` or a command of its own leaves it on the overlay; a
// declared list is any service's, sizes as given; a bad entry is refused.
func TestServiceTmpfs(t *testing.T) {
	tests := []struct {
		name, body string
		want       []Tmpfs
	}{
		{"tuned postgres, default PGDATA", "db:\n  image: postgres:17-alpine\n", []Tmpfs{{Path: "/tmp"}}},
		{"tuned postgres, own PGDATA", "db:\n  image: postgis/postgis:16\n  env:\n    PGDATA: /data/pg/main\n", []Tmpfs{{Path: "/data/pg"}}},
		{"opted out", "db:\n  image: postgres:17\n  tmpfs: false\n", nil},
		{"not tuned", "db:\n  image: postgres:17\n  tuning: false\n", nil},
		{"own command", "db:\n  image: postgres:17\n  command: [-c, max_connections=200]\n", nil},
		{"declared, any service", "cache:\n  image: redis:7\n  tmpfs: [/data, \"/scratch:512m\"]\n", []Tmpfs{{Path: "/data"}, {Path: "/scratch", Size: "512m"}}},
		{"declared wins over the default", "db:\n  image: postgres:17\n  tmpfs: [\"/var/lib/postgresql:1g\"]\n", []Tmpfs{{Path: "/var/lib/postgresql", Size: "1g"}}},
		{"redis gets none", "cache:\n  image: redis:7\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveFile(t, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got[0].Tmpfs, tt.want) {
				t.Fatalf("tmpfs %+v, want %+v", got[0].Tmpfs, tt.want)
			}
			noted := false
			for _, n := range got[0].Notes {
				noted = noted || strings.Contains(n, "data on a tmpfs at")
			}
			if isDefault := tt.want != nil && !strings.Contains(tt.body, "tmpfs:"); noted != isDefault {
				t.Fatalf("notes %q: a default tmpfs noted %v, want %v", got[0].Notes, noted, isDefault)
			}
		})
	}
	for _, bad := range []string{"tmpfs: true", "tmpfs: [relative]", "tmpfs: [\"/x:lots\"]", "tmpfs: [\"/x,y\"]"} {
		if _, err := resolveFile(t, "db:\n  image: postgres:17\n  "+bad+"\n"); err == nil || !strings.Contains(err.Error(), "tmpfs") {
			t.Fatalf("%s: err %v, want a refusal naming tmpfs", bad, err)
		}
	}
}

// A service's tmpfs reaches podman as --tmpfs <path>:rw,size=<n>,mode=1777,
// a path naming no size taking the node's services_tmpfs (default 4g);
// none is no flag at all.
func TestSidecarTmpfsArgs(t *testing.T) {
	pod := Pod{Name: "mesh-pr-1-ab-0"}
	c := Config{}
	svc := Service{Name: "postgres", Image: "postgres:17", Tmpfs: []Tmpfs{{Path: "/tmp"}, {Path: "/scratch", Size: "512m"}}}
	args := strings.Join(SidecarRunArgs(pod, SidecarSpec{Name: svc.Name, Image: svc.Image, Tmpfs: c.tmpfsOf(svc)}), " ")
	for _, want := range []string{"--tmpfs /tmp:rw,size=4g,mode=1777", "--tmpfs /scratch:rw,size=512m,mode=1777"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args %q lack %q", args, want)
		}
	}
	if i, j := strings.Index(args, "--tmpfs"), strings.Index(args, "postgres:17"); i > j {
		t.Fatalf("--tmpfs after the image: %q", args)
	}
	c.ServicesTmpfs = "2g"
	if got := c.tmpfsOf(Service{Tmpfs: []Tmpfs{{Path: "/tmp"}}}); got[0].Size != "2g" {
		t.Fatalf("size %q, want the node's 2g", got[0].Size)
	}
	if args := SidecarRunArgs(pod, SidecarSpec{Name: "redis", Image: "redis:7", Tmpfs: c.tmpfsOf(Service{})}); strings.Contains(strings.Join(args, " "), "--tmpfs") {
		t.Fatalf("a service without tmpfs got one: %q", args)
	}
}

// Start hands each started sidecar its tmpfs, sized.
func TestStartGivesASidecarItsTmpfs(t *testing.T) {
	f := &Fake{}
	sb := testSandbox(t, f)
	sb.Config.ServicesTmpfs = "3g"
	sess, err := sb.Open("pr-1", "validate", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Start(context.Background(), []Service{{Name: "postgres", Image: "postgres:17", Tmpfs: []Tmpfs{{Path: "/tmp"}}}}, 0); err != nil {
		t.Fatal(err)
	}
	if len(f.Sidecars) != 1 || !reflect.DeepEqual(f.Sidecars[0].Tmpfs, []Tmpfs{{Path: "/tmp", Size: "3g"}}) {
		t.Fatalf("sidecars %+v", f.Sidecars)
	}
}
