package providers

import (
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

var images = map[string]spec.Image{"api": {Context: ".", Dockerfile: "Dockerfile"}}

func prov(name string, kv ...any) spec.Provider {
	p := spec.Provider{Name: name, Config: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		p.Config[kv[i].(string)] = kv[i+1]
	}
	return p
}

// check runs fn and asserts it fails with want, or succeeds when want is "".
func check(t *testing.T, name string, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: unexpected error %v", name, err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Errorf("%s: error %v, want %q", name, err, want)
	}
}

func TestRegistryAuth(t *testing.T) {
	check(t, "token", RegistryAuth(prov("token", "user", "u", "token_env", "T"), true), "")
	check(t, "token no user", RegistryAuth(prov("token", "token_env", "T"), false), "user is required")
	check(t, "token env", RegistryAuth(prov("token", "user", "u", "token_env", "t"), false), "token_env")
	check(t, "docker-config", RegistryAuth(prov("docker-config"), true), "")
	check(t, "none pull", RegistryAuth(prov("none"), false), "")
	check(t, "none push", RegistryAuth(prov("none"), true), "publishing needs credentials")
	check(t, "missing", RegistryAuth(spec.Provider{}, false), "provider is required")
	check(t, "mtls", RegistryAuth(prov("mtls"), true), "unknown provider")
	check(t, "unknown key", RegistryAuth(prov("token", "user", "u", "token_env", "T", "cert", "x"), true), "cert")
}

func TestSigning(t *testing.T) {
	check(t, "off", Signing(nil), "")
	p := prov("cosign-key", "key", "env://K", "public_keys", []any{"a.pub"})
	check(t, "cosign", Signing(&p), "")
	p = prov("cosign-key", "public_keys", []any{"a.pub"})
	check(t, "no key", Signing(&p), "key is required")
	p = prov("cosign-key", "key", "k")
	check(t, "no public keys", Signing(&p), "public_keys is required")
	p = prov("gpg")
	check(t, "unknown", Signing(&p), "unknown provider")
}

func TestHealth(t *testing.T) {
	ok := []any{"service", "api", "port", 8000, "path", "/ready"}
	svc, err := Health(prov("http-json", append(ok, "expect", map[string]any{"status": "ready"})...), images)
	check(t, "http-json", err, "")
	if svc != "api" {
		t.Errorf("service = %q", svc)
	}
	_, err = Health(prov("http-json", ok...), images)
	check(t, "http-json no expect", err, "expect is required")
	_, err = Health(prov("http-status", ok...), images)
	check(t, "http-status default 200", err, "")
	_, err = Health(prov("http-status", append(ok, "expect", 500)...), images)
	check(t, "http-status 500", err, "not a success status")
	// Both sides of the TCP port range.
	for _, port := range []int{0, 65536} {
		_, err = Health(prov("http-status", "service", "api", "port", port, "path", "/r"), images)
		check(t, "port", err, "not a TCP port")
	}
	for _, port := range []int{1, 65535} {
		_, err = Health(prov("http-status", "service", "api", "port", port, "path", "/r"), images)
		check(t, "port", err, "")
	}
	_, err = Health(prov("http-status", "port", 1, "path", "/r"), images)
	check(t, "no service", err, "service is required")
	_, err = Health(prov("http-status", append(ok, "liveness_path", "health")...), images)
	check(t, "liveness path", err, "liveness_path")
	_, err = Health(prov("command", "image", "api", "cmd", []any{"x"}), images)
	check(t, "command", err, "")
	_, err = Health(prov("command", "image", "web", "cmd", []any{"x"}), images)
	check(t, "command image", err, "not an images key")
	_, err = Health(prov("command", "image", "api"), images)
	check(t, "command cmd", err, "cmd is required")
	_, err = Health(prov("command", "cmd", []any{"x"}), images)
	check(t, "command no image", err, "image is required")
	_, err = Health(prov("command", "image", "api", "cmd", []any{"x"}, "timeout_s", -1), images)
	check(t, "command timeout", err, "timeout_s")
	_, err = Health(spec.Provider{}, images)
	check(t, "missing", err, "provider is required")
	_, err = Health(prov("tcp"), images)
	check(t, "unknown", err, "unknown provider")
}

func TestSchemaCompat(t *testing.T) {
	check(t, "sql", SchemaCompat(prov("sql", "current", "SELECT 1", "barrier_after", "SELECT $1"), images), "")
	check(t, "sql no current", SchemaCompat(prov("sql"), images), "current is required")
	check(t, "sql barrier", SchemaCompat(prov("sql", "current", "x", "barrier_after", "SELECT 1"), images), "$1")
	check(t, "sql reads newer", SchemaCompat(prov("sql", "current", "x", "reads_newer_label", "l", "reads_newer_image", "api"), images), "")
	check(t, "sql label alone", SchemaCompat(prov("sql", "current", "x", "reads_newer_label", "l"), images), "go together")
	check(t, "sql image alone", SchemaCompat(prov("sql", "current", "x", "reads_newer_image", "api"), images), "go together")
	check(t, "sql label image", SchemaCompat(prov("sql", "current", "x", "reads_newer_label", "l", "reads_newer_image", "web"), images), "not an images key")
	check(t, "sql baseline", SchemaCompat(prov("sql", "current", "x", "baseline", 1), images), "")
	check(t, "sql no baseline", SchemaCompat(prov("sql", "current", "x", "baseline", 0), images), "")
	check(t, "sql negative baseline", SchemaCompat(prov("sql", "current", "x", "baseline", -1), images), "baseline -1")
	for _, c := range []struct {
		p    spec.Provider
		want int
		err  bool
	}{
		{prov("sql", "current", "x", "baseline", 2), 2, false},
		{prov("sql", "current", "x", "baseline", 1), 1, false},
		{prov("sql", "current", "x"), 1, false},
		{prov("none"), 1, false},
		{prov("sql", "current", "x", "baseline", -1), 0, true},
		{prov("sql", "current", "x", "baseline", "two"), 0, true},
		{prov("sql", "current", "x", "baseline", 2.5), 0, true},
		// Read alone: a config the provider refuses for another reason keeps its baseline.
		{prov("sql", "barrier_after", "no param", "baseline", 3), 3, false},
	} {
		b, err := SchemaBaseline(c.p)
		if b != c.want || (err != nil) != c.err {
			t.Errorf("SchemaBaseline(%v) = %d, %v", c.p.Config, b, err)
		}
	}
	check(t, "command", SchemaCompat(prov("command", "image", "api", "cmd", []any{"x"}), images), "")
	check(t, "none", SchemaCompat(prov("none"), images), "")
	check(t, "none with config", SchemaCompat(prov("none", "x", 1), images), "x")
	check(t, "missing", SchemaCompat(spec.Provider{}, images), "provider is required")
	check(t, "unknown", SchemaCompat(prov("flyway"), images), "unknown provider")
}

func TestProvision(t *testing.T) {
	check(t, "off", Provision("notes", nil, images), "")
	p := prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{"ADMIN_EMAIL"}, "outputs", "creds.txt")
	check(t, "container", Provision("notes", &p, images), "")
	p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{})
	check(t, "env empty", Provision("notes", &p, images), "")
	p = prov("container", "image", "api", "cmd", []any{"x"})
	check(t, "env missing", Provision("notes", &p, images), "env is required")
	p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{"admin"})
	check(t, "env name", Provision("notes", &p, images), "not a variable name")
	p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{"A", "A"})
	check(t, "env twice", Provision("notes", &p, images), "listed twice")
	p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{}, "returns", []any{"ADMIN_KEY", "ADMIN_LOGIN"})
	check(t, "returns", Provision("notes", &p, images), "")
	p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{}, "returns", []any{"admin_key"})
	check(t, "returns name", Provision("notes", &p, images), `returns: "admin_key" is not a variable name`)
	p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{}, "returns", []any{"K", "K"})
	check(t, "returns twice", Provision("notes", &p, images), "returns: K is listed twice")
	for _, k := range []string{"STOWAGE_URL_API", "COMPOSE_PROJECT_NAME"} {
		p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{}, "returns", []any{k})
		check(t, "returns "+k, Provision("notes", &p, images), "returns: "+k+" is a value the install records")
	}
	p = prov("container", "image", "web", "cmd", []any{"x"}, "env", []any{})
	check(t, "image", Provision("notes", &p, images), "not an images key")
	p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{}, "outputs", "a/b")
	check(t, "outputs", Provision("notes", &p, images), "file name")
	for _, name := range []string{".env", "compose.yaml", "provision.env", "stowage-front.yaml", "snapshots", "notes-stow", "a\nb"} {
		p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{}, "outputs", name)
		check(t, "outputs "+name, Provision("notes", &p, images), "not one the install writes")
	}
	p = prov("container", "image", "api", "cmd", []any{"x"}, "env", []any{}, "outputs", "env.txt")
	check(t, "outputs env.txt", Provision("notes", &p, images), "")
	p = prov("script")
	check(t, "unknown", Provision("notes", &p, images), "unknown provider")
}

func TestBackup(t *testing.T) {
	check(t, "off", Backup(nil, images), "")
	cases := []struct {
		name string
		p    spec.Provider
		want string
	}{
		{"one profile", prov("pg-age-rclone", "profiles", map[string]any{"full": "all"}), ""},
		{"no profiles", prov("pg-age-rclone"), "profiles is required"},
		{"bad string", prov("pg-age-rclone", "profiles", map[string]any{"full": "some"}), "\"all\", a table list, or { exclude"},
		{"empty list", prov("pg-age-rclone", "profiles", map[string]any{"full": []any{}}), "lists no table"},
		{"non-string table", prov("pg-age-rclone", "profiles", map[string]any{"full": []any{1}}), "non-string table"},
		{"bad type", prov("pg-age-rclone", "profiles", map[string]any{"full": 3}), "\"all\", a table list, or { exclude"},
		{"exclude", prov("pg-age-rclone", "profiles", map[string]any{"lean": map[string]any{"exclude": []any{"events", "audit.log_*", "t?"}}}), ""},
		{"exclude none", prov("pg-age-rclone", "profiles", map[string]any{"lean": map[string]any{"exclude": []any{}}}), "or { exclude: [pattern, …] }"},
		{"exclude and more", prov("pg-age-rclone", "profiles", map[string]any{"lean": map[string]any{"exclude": []any{"t"}, "tables": []any{"u"}}}), "or { exclude: [pattern, …] }"},
		{"exclude a quote", prov("pg-age-rclone", "profiles", map[string]any{"lean": map[string]any{"exclude": []any{`t"; DROP`}}}), "is not a table pattern"},
		{"exclude three parts", prov("pg-age-rclone", "profiles", map[string]any{"lean": map[string]any{"exclude": []any{"a.b.c"}}}), "is not a table pattern"},
		{"default needed", prov("pg-age-rclone", "profiles", map[string]any{"a": "all", "b": []any{"t"}}), "default_profile is required"},
		{"default unknown", prov("pg-age-rclone", "default_profile", "c", "profiles", map[string]any{"a": "all"}), "is not a profile"},
		{"profile with a dash", prov("pg-age-rclone", "profiles", map[string]any{"full-x": "all"}), "lowercase letters and digits"},
		{"profile in capitals", prov("pg-age-rclone", "profiles", map[string]any{"Full": "all"}), "lowercase letters and digits"},
		{"profile of 32", prov("pg-age-rclone", "profiles", map[string]any{strings.Repeat("a", 32): "all"}), ""},
		{"profile of 33", prov("pg-age-rclone", "profiles", map[string]any{strings.Repeat("a", 33): "all"}), "lowercase letters and digits"},
		{"volumes", prov("pg-age-rclone", "profiles", map[string]any{"full": "all"}, "volumes", []any{"uploads", "keys.d_1"}), ""},
		{"volume not a key", prov("pg-age-rclone", "profiles", map[string]any{"full": "all"}, "volumes", []any{"../uploads"}), `volumes[0] "../uploads" is not a compose volume's key`},
		{"volume twice", prov("pg-age-rclone", "profiles", map[string]any{"full": "all"}, "volumes", []any{"a", "b", "a"}), `volumes[2] "a" is listed twice`},
		{"command", prov("command", "image", "api", "cmd", []any{"x"}), ""},
		{"unknown", prov("restic"), "unknown provider"},
	}
	for _, c := range cases {
		check(t, c.name, Backup(&c.p, images), c.want)
	}
}

// With several bad profiles, the same one is reported every time.
func TestBackupReportsDeterministically(t *testing.T) {
	p := prov("pg-age-rclone", "default_profile", "a", "profiles", map[string]any{"a": 1, "b": 2, "c": 3, "d": 4})
	first := Backup(&p, images).Error()
	for range 20 {
		if got := Backup(&p, images).Error(); got != first {
			t.Fatalf("%q then %q", first, got)
		}
	}
}
