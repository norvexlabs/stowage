package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/testutil"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

const (
	specFile    = "stowage.yaml"
	composeFile = "deploy/compose.yaml"
)

func run(t *testing.T, root string) *Result {
	t.Helper()
	res, err := Run(root, filepath.Join(root, specFile), now)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func errorsOf(r *Result) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Severity == Error {
			out = append(out, f.String())
		}
	}
	return out
}

// The positive control every negative case below depends on: the unmodified examples are clean.
// The full example uses every spec key, so a rule that misfires on a valid key fails here.
func TestExamplesAreClean(t *testing.T) {
	for name, version := range map[string]string{"minimal": "1.0.0", "full": "2026.10.1", "stateless": "1.0.0"} {
		t.Run(name, func(t *testing.T) {
			res := run(t, testutil.CopyExample(t, name))
			if errs := errorsOf(res); len(errs) > 0 {
				t.Fatalf("%s example has errors:\n%s", name, strings.Join(errs, "\n"))
			}
			if res.Version != version {
				t.Errorf("Version = %q, want %q", res.Version, version)
			}
		})
	}
}

func TestRegistryPrefixes(t *testing.T) {
	t.Parallel()
	for p, ok := range map[string]bool{
		"ghcr.io/acme/notes": true, "registry.lan:5000/app": true, "[fd00::1]:5000/app": true, "[::1]/app": true,
		"[fd00::1": false, "[g::1]/app": false, "fd00::1:5000/app": false, "[]/app": false, "Ghcr.io/acme": false,
	} {
		if prefixRe.MatchString(p) != ok {
			t.Errorf("%s: want %v", p, ok)
		}
	}
}

func TestEachRuleFires(t *testing.T) {
	type edit struct{ file, old, new string }
	cases := []struct {
		name  string
		edits []edit
		write map[string]string
		want  string
	}{
		{"bad name", []edit{{specFile, "\nname: notes\n", "\nname: Notes\n"}}, nil, "name:"},
		{"bad short name", []edit{{specFile, "\nname: notes\n", "\nname: notes\nshort_name: No\n"}}, nil, "short_name:"},
		{"stowage short name", []edit{{specFile, "\nname: notes\n", "\nname: notes\nshort_name: stowage-notes\n"}}, nil, "keeps for Stowage's own files"},
		{"compose build", []edit{{composeFile, "image: ${STOWAGE_IMAGE_WEB}", "image: ${STOWAGE_IMAGE_WEB}\n    build: ./web"}}, nil, "build: is forbidden"},
		{"compose name", []edit{{composeFile, "services:", "name: notes\nservices:"}}, nil, "top-level name"},
		{"literal image", []edit{{composeFile, "image: ${STOWAGE_IMAGE_WEB}", "image: nginx:latest"}}, nil, "single deployment point"},
		{"image var of unknown key", []edit{{composeFile, "image: ${STOWAGE_IMAGE_WEB}", "image: ${STOWAGE_IMAGE_NOPE}"}}, nil, "for a key of images"},
		{"safe default relaxed", []edit{{composeFile, "${SECURE_COOKIES:-true}", "${SECURE_COOKIES:-false}"}}, nil, "${SECURE_COOKIES:-true}"},
		{"safe default without default", []edit{{composeFile, "${SECURE_COOKIES:-true}", "${SECURE_COOKIES}"}}, nil, "${SECURE_COOKIES:-true}"},
		{"safe default unused", []edit{{composeFile, "      SECURE_COOKIES: ${SECURE_COOKIES:-true}\n", ""}}, nil, "not used in"},
		{"undeclared variable", []edit{{composeFile, "${LOG_LEVEL:-info}", "${LOG_LEVEL}"}}, nil, "${LOG_LEVEL} has no default"},
		{"unknown STOWAGE_ var", []edit{{composeFile, "${LOG_LEVEL:-info}", "${STOWAGE_NOPE:-x}"}}, nil, "not a variable Stowage provides"},
		{"reserved image key", []edit{{specFile, "  web: { context: web", "  release: { context: web"}, {composeFile, "${STOWAGE_IMAGE_WEB}", "${STOWAGE_IMAGE_RELEASE}"}}, nil, "is reserved"},
		{"backup image key", []edit{{specFile, "  web: { context: web", "  backup: { context: web"}, {composeFile, "${STOWAGE_IMAGE_WEB}", "${STOWAGE_IMAGE_BACKUP}"}}, nil, `"backup" is reserved`},
		{"front image key", []edit{{specFile, "  web: { context: web", "  front: { context: web"}, {composeFile, "${STOWAGE_IMAGE_WEB}", "${STOWAGE_IMAGE_FRONT}"}}, nil, `"front" is reserved`},
		{"unpinned ref", []edit{{specFile, "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24", "postgres:17-alpine"}}, nil, "pinned by digest"},
		{"missing dockerfile", []edit{{specFile, "dockerfile: web/Dockerfile", "dockerfile: web/Nope"}}, nil, "web/Nope"},
		{"missing input", []edit{{specFile, "inputs: [web/]", "inputs: [nope/]"}}, nil, "nope/"},
		{"route upstream not a service", []edit{{specFile, "upstream: web:8080", "upstream: www:8080"}}, nil, `service "www"`},
		{"portal names no route", []edit{{specFile, "portal: app", "portal: home"}}, nil, "names no route"},
		{"duplicate domain", []edit{{specFile, "domain_input: API_DOMAIN", "domain_input: APP_DOMAIN"}}, nil, "routing is by hostname"},
		{"unknown tls mode", []edit{{specFile, "tls_modes: [acme, byo, internal, none]", "tls_modes: [acme, letsencrypt]"}}, nil, "unknown mode"},
		{"health service missing", []edit{{specFile, "  service: api\n  port: 8000", "  service: backend\n  port: 8000"}}, nil, `service "backend"`},
		{"health bad path", []edit{{specFile, "path: /healthz/ready", "path: healthz"}}, nil, "must start with /"},
		{"database service missing", []edit{{specFile, "  service: postgres\n  name: notes", "  service: pg\n  name: notes"}}, nil, `service "pg"`},
		{"reset unknown volume", []edit{{specFile, "reset: [postgres, uploads]", "reset: [postgres, cache]"}}, nil, "not a data volume"},
		{"unknown secret generator", []edit{{specFile, "gen: pass18", "gen: pass99"}}, nil, "unknown generator"},
		{"duplicate key", []edit{{specFile, "key: SESSION_SECRET", "key: POSTGRES_PASSWORD"}}, nil, "already declared"},
		{"reserved prefix", []edit{{specFile, "key: ADMIN_EMAIL", "key: STOWAGE_ADMIN"}}, nil, "reserved"},
		{"secret input with default", []edit{{specFile, `prompt: "Administrator email", required: true`, `prompt: "x", secret: true, default: "y"`}}, nil, "secret input"},
		{"provision image unknown", []edit{{specFile, "    image: api\n    cmd: [notes-api, create-admin]", "    image: tools\n    cmd: [notes-api, create-admin]"}}, nil, "not an images key"},
		{"schema compat unknown key", []edit{{specFile, "    image: api                                                 # runs inside the api image", "    image: api\n    shell: true"}}, nil, "shell"},
		{"push none", []edit{{specFile, "push: { provider: docker-config }", "push: { provider: none }"}}, nil, "publishing needs credentials"},
		{"prefix with scheme", []edit{{specFile, "prefix: ghcr.io/acme/notes", "prefix: https://ghcr.io/acme/notes"}}, nil, "no scheme"},
		{"bad channel template", []edit{{specFile, `beta:   { tag: "beta-{v}", mutable: true }                 # a suffix would collide with SemVer pre-releases`, `beta:   { tag: "beta", mutable: true }`}}, nil, "{v} exactly once"},
		{"channel tags collide", []edit{{specFile, `beta:   { tag: "beta-{v}", mutable: true }`, `beta:   { tag: "{v}-beta", mutable: true }`}}, nil, "also channel stable's tag for version 1.0.0-beta"},
		{"two latest", []edit{{specFile, `beta:   { tag: "beta-{v}", mutable: true }`, `beta:   { tag: "beta-{v}", mutable: true, latest: true }`}}, nil, "only one channel"},
		{"version not in scheme", nil, map[string]string{"VERSION": "v1\n"}, "MAJOR.MINOR.PATCH"},
		{"no changelog section", nil, map[string]string{"VERSION": "1.1.0\n"}, "no section for the current version 1.1.0"},
		{"chart without Chart.yaml", []edit{{specFile, "chart: deploy/chart", "chart: api"}}, nil, "Chart.yaml"},
		{"chart values reserved key", nil, map[string]string{"deploy/chart/values.yaml": "stowage: {}\n"}, "shapes.kubernetes.chart: deploy/chart: values.yaml: the key stowage is reserved"},
		{"chart apiVersion v1", []edit{{"deploy/chart/Chart.yaml", "apiVersion: v2", "apiVersion: v1"}}, nil, "shapes.kubernetes.chart: deploy/chart: Chart.yaml: apiVersion"},
		{"chart library", nil, map[string]string{"deploy/chart/Chart.yaml": "apiVersion: v2\nname: notes\ntype: library\n"}, "library chart cannot be installed"},
		{"chart dependency not vendored", nil, map[string]string{"deploy/chart/Chart.yaml": "apiVersion: v2\nname: notes\ndependencies:\n  - name: db\n    version: 1.0.0\n"}, "dependencies db are not vendored"},
		{"chart values not extendable", nil, map[string]string{"deploy/chart/values.yaml": "{web: {replicas: 1}}\n"}, "adding the stowage block"},
		{"unknown disk format", []edit{{specFile, "disk_formats: [qcow2]", "disk_formats: [vmdk]"}}, nil, "unknown format"},
		{"bad lock", nil, map[string]string{"stowage.lock": "stowage: 1.0.0\nruntime: {}\n"}, "field runtime not found"},
		{"backup default profile", []edit{{specFile, "profiles: { full: all }", "profiles: { full: all, config: [users] }"}}, nil, "default_profile is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := testutil.CopyExample(t, "minimal")
			for _, e := range c.edits {
				testutil.Edit(t, root, e.file, e.old, e.new)
			}
			for f, content := range c.write {
				testutil.Write(t, root, f, content)
			}
			errs := errorsOf(run(t, root))
			for _, e := range errs {
				if strings.Contains(e, c.want) {
					return
				}
			}
			t.Fatalf("no error containing %q; got:\n%s", c.want, strings.Join(errs, "\n"))
		})
	}
}

// Every rule not reached from the minimal example, each by one mutation of the full example.
func TestEachRuleFiresOnFull(t *testing.T) {
	const sp, cp = specFile, composeFile
	type edit struct{ file, old, new string }
	cases := []struct {
		name  string
		edits []edit
		write map[string]string
		want  string
	}{
		{"absolute path", []edit{{sp, "compose: deploy/compose.yaml", "compose: /etc/compose.yaml"}}, nil, "must be relative to the project root"},
		{"hook env not recorded", []edit{{sp, "env: [ADMIN_EMAIL, ADMIN_PASSWORD, INTERNAL_SERVICE_SECRET]", "env: [ADMIN_EMAIL, API_URL]"}}, nil, "hooks.provision.env[1]: API_URL is no input, secret or safe default"},
		{"hook returns an input", []edit{{sp, "returns: [ADMIN_API_KEY, ADMIN_LOGIN]", "returns: [ADMIN_API_KEY, ADMIN_EMAIL]"}}, nil, "hooks.provision.returns[1]: ADMIN_EMAIL is an input, a value the install records"},
		{"hook returns a secret", []edit{{sp, "returns: [ADMIN_API_KEY, ADMIN_LOGIN]", "returns: [INTERNAL_SERVICE_SECRET]"}}, nil, "hooks.provision.returns[0]: INTERNAL_SERVICE_SECRET is a secret"},
		{"hook returns a safe default", []edit{{sp, "returns: [ADMIN_API_KEY, ADMIN_LOGIN]", "returns: [COOKIE_SECURE]"}}, nil, "hooks.provision.returns[0]: COOKIE_SECURE is a safe default"},
		{"hook returns a route domain", []edit{{sp, "returns: [ADMIN_API_KEY, ADMIN_LOGIN]", "returns: [API_DOMAIN]"}}, nil, "hooks.provision.returns[0]: API_DOMAIN is a route domain"},
		{"hook returns twice", []edit{{sp, "returns: [ADMIN_API_KEY, ADMIN_LOGIN]", "returns: [ADMIN_LOGIN, ADMIN_LOGIN]"}}, nil, "hooks.provision: container: returns: ADMIN_LOGIN is listed twice"},
		{"hook returns a bad name", []edit{{sp, "returns: [ADMIN_API_KEY, ADMIN_LOGIN]", "returns: [admin-key]"}}, nil, "hooks.provision: container: returns: \"admin-key\" is not a variable name"},
		{"hook returns a provided value", []edit{{sp, "returns: [ADMIN_API_KEY, ADMIN_LOGIN]", "returns: [STOWAGE_TLS_MODE]"}}, nil, "hooks.provision: container: returns: STOWAGE_TLS_MODE is a value the install records"},
		{"hook env missing", []edit{{sp, "    env: [ADMIN_EMAIL, ADMIN_PASSWORD, INTERNAL_SERVICE_SECRET]   # all it receives, with what it returned before\n", ""}}, nil, "hooks.provision: container: env is required"},
		{"version file outside", []edit{{sp, "source: { provider: changelog, file: CHANGELOG.md }", "source: { provider: changelog, file: ../../CHANGELOG.md }"}}, nil, "version.source file: \"../../CHANGELOG.md\" leaves the project root"},
		{"changelog file absolute", []edit{{sp, "changelog: { provider: keepachangelog, file: CHANGELOG.md }", "changelog: { provider: keepachangelog, file: /etc/motd }"}}, nil, "changelog file: \"/etc/motd\" must be relative"},
		{"escaping path", []edit{{sp, "compose: deploy/compose.yaml", "compose: ../compose.yaml"}}, nil, "leaves the project root"},
		{"context is a file", []edit{{sp, "context: ui, dockerfile", "context: ui/index.html, dockerfile"}}, nil, "is not a directory"},
		{"dockerfile is a dir", []edit{{sp, "dockerfile: ui/Dockerfile", "dockerfile: ui"}}, nil, "is not a file"},
		{"unknown scheme", []edit{{sp, "scheme: { provider: calver,", "scheme: { provider: pep440,"}}, nil, "unknown provider \"pep440\""},
		{"unknown source", []edit{{sp, "source: { provider: changelog,", "source: { provider: svn,"}}, nil, "unknown provider \"svn\""},
		{"unknown changelog", []edit{{sp, "changelog: { provider: keepachangelog,", "changelog: { provider: news,"}}, nil, "unknown provider \"news\""},
		{"version source fails", []edit{{sp, "source: { provider: changelog, file: CHANGELOG.md }", "source: { provider: changelog, file: NOPE.md }"}}, nil, "NOPE.md"},
		{"no channel", []edit{{sp, "channels:\n  stable: { tag: \"v{v}\", mutable: false, latest: true }       # moves installer :latest\n  dev:    { tag: \"v{v}-dev\", mutable: true }\n", "channels: {}\n"}}, nil, "at least one channel"},
		{"bad channel name", []edit{{sp, "  dev:    {", "  Dev:    {"}}, nil, "channel name"},
		{"no prefix", []edit{{sp, "  prefix: registry.example.com/atlas\n", ""}}, nil, "single deployment point"},
		{"pull token_env", []edit{{sp, "token_env: STOWAGE_PULL_TOKEN", "token_env: pull-token"}}, nil, "registry.pull: token: token_env"},
		{"push provider missing", []edit{{sp, "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }", "push: { user: publisher }"}}, nil, "registry.push: provider is required"},
		{"signing without public keys", []edit{{sp, "  public_keys: [keys/release-2026.pub]\n", ""}}, nil, "public_keys is required"},
		{"loopback env not a safe default", []edit{{sp, `loopback_env: { COOKIE_SECURE: "false",`, `loopback_env: { DEBUG: "1", COOKIE_SECURE: "false",`}}, nil, "front.loopback_env.DEBUG: is not a safe_defaults key"},
		{"route without loopback port", []edit{{sp, "loopback_ports: { console: 3000, api: 9090, ingest: 8080 }", "loopback_ports: { console: 3000, api: 9090 }"}}, nil, "route ingest has no port"},
		{"duplicate loopback port", []edit{{sp, "ingest: 8080 }", "ingest: 9090 }"}}, nil, "is already api's port"},
		{"reserved service name", nil, map[string]string{cp: "services:\n  stowage-front:\n    image: ${STOWAGE_IMAGE_API}\n"}, "names starting stowage- are Stowage's own"},
		{"reserved volume name", nil, map[string]string{cp: "services:\n  api:\n    image: ${STOWAGE_IMAGE_API}\nvolumes:\n  stowage-front-data: {}\n"}, "volumes.stowage-front-data: names starting stowage- are Stowage's own"},
		{"compose variable claimed", []edit{{sp, "  COOKIE_SECURE: \"true\"\n", "  COOKIE_SECURE: \"true\"\n  COMPOSE_PROFILES: \"x\"\n"}}, nil, "COMPOSE_PROFILES: the COMPOSE_ prefix is reserved"},
		{"process variable claimed", []edit{{sp, "  COOKIE_SECURE: \"true\"\n", "  COOKIE_SECURE: \"true\"\n  HOME: \"/\"\n"}}, nil, "HOME: the Docker client reads it"},
		{"unrecordable loopback value", []edit{{sp, `loopback_env: { COOKIE_SECURE: "false"`, `loopback_env: { COOKIE_SECURE: "it's false"`}}, nil, "front.loopback_env.COOKIE_SECURE: a value cannot hold a single quote"},
		{"routes sharing a variable", []edit{{sp, "- name: api", "- name: in_gest"}, {sp, "- name: ingest", "- name: in-gest"}, {sp, "loopback_ports: { console: 3000, api: 9090, ingest: 8080 }", "loopback_ports: { console: 3000, in_gest: 9090, in-gest: 8080 }"}}, nil, `"in_gest" and "in-gest" both give STOWAGE_URL_IN_GEST`},
		{"url of no route", []edit{{cp, "${STOWAGE_URL_INGEST}", "${STOWAGE_URL_INGRESS}"}}, nil, "STOWAGE_URL_INGRESS names no front route"},
		{"public key missing", []edit{{sp, "public_keys: [keys/release-2026.pub]", "public_keys: [keys/nope.pub]"}}, nil, "signing.public_keys[0]: keys/nope.pub"},
		{"public key not PEM", nil, map[string]string{"keys/release-2026.pub": "ssh-ed25519 AAAA\n"}, "is not a PEM public key"},
		{"public key unreadable DER", nil, map[string]string{"keys/release-2026.pub": "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"}, "signing.public_keys[0]: keys/release-2026.pub: "},
		{"image key with underscore", []edit{{sp, "  ui:        {", "  u_i:       {"}}, nil, "separated by single '-'"},
		{"ref and build", []edit{{sp, `postgres:  { ref:`, `postgres:  { context: ., ref:`}}, nil, "exclusive"},
		{"unpinned FROM", []edit{{"provision/Dockerfile", "FROM python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a", "FROM python:3.13-alpine"}}, nil, "FROM python:3.13-alpine must be pinned"},
		{"invalid Dockerfile", []edit{{"provision/Dockerfile", "FROM python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a", "FROM python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a AS a b"}}, nil, "provision/Dockerfile: dockerfile:"},
		{"unknown target", []edit{{sp, "target: runtime", "target: final"}}, nil, `"final" is not a stage`},
		{"compose no service", nil, map[string]string{cp: "services: {}\n"}, "declares no service"},
		{"compose unparsable", nil, map[string]string{cp: "services: [\n"}, "stack.compose"},
		{"unterminated interpolation", []edit{{cp, "${LOG_LEVEL:-info}", "${LOG_LEVEL:-info"}}, nil, "unterminated"},
		{"no tls mode", []edit{{sp, "tls_modes: [acme, dns-cloudflare, byo, internal, none]", "tls_modes: []"}}, nil, "at least one TLS mode"},
		{"bad route name", []edit{{sp, "- name: ingest", "- name: Ingest"}}, nil, "front.routes[2].name"},
		{"forward auth without address", []edit{{sp, "forward_auth: { address_input: AUTH_ADDRESS, ", "forward_auth: { "}}, nil, "front.routes[1].forward_auth.address_input: is required"},
		{"forward auth address no input", []edit{{sp, "address_input: AUTH_ADDRESS", "address_input: AUTH_HOST"}}, nil, "forward_auth.address_input: AUTH_HOST is not an input"},
		{"forward auth uri no input", []edit{{sp, "uri_input: AUTH_URI", "uri_input: AUTH_PATH"}}, nil, "forward_auth.uri_input: AUTH_PATH is not an input"},
		{"copy header not a name", []edit{{sp, "copy_headers: [X-Auth-Request-User]", "copy_headers: [X-Auth-Request-User:]"}}, nil, `forward_auth.copy_headers: "X-Auth-Request-User:" is not a header name`},
		{"allow input no input", []edit{{sp, "allow_input: INGEST_SOURCES", "allow_input: SOURCES"}}, nil, "front.routes[2].allow_input: SOURCES is not an input"},
		{"allow input not on docker", []edit{{sp, `default: "0.0.0.0/0, ::/0", shapes: [docker, kairos]`, `default: "0.0.0.0/0, ::/0", shapes: [kubernetes]`}}, nil, "INGEST_SOURCES is not an input of the docker shape"},
		{"allow input not on kairos", []edit{{sp, `default: "0.0.0.0/0, ::/0", shapes: [docker, kairos]`, `default: "0.0.0.0/0, ::/0", shapes: [docker]`}}, nil, "INGEST_SOURCES is not an input of the kairos shape"},
		{"allow input asked in one mode", []edit{{sp, `default: "0.0.0.0/0, ::/0", shapes`, `default: "0.0.0.0/0, ::/0", when: tls, shapes`}}, nil, "INGEST_SOURCES is asked only when tls"},
		{"allow input advanced", []edit{{sp, `default: "0.0.0.0/0, ::/0", shapes`, `advanced: true, shapes`}}, nil, "INGEST_SOURCES is advanced, never asked"},
		{"allow input required", []edit{{sp, `default: "0.0.0.0/0, ::/0", shapes`, `required: true, shapes`}}, nil, "INGEST_SOURCES is required: empty is how the front"},
		{"publish ca volume undeclared", []edit{{sp, "publish_ca: { volume: ca }", "publish_ca: { volume: cacerts }"}}, nil, `front.publish_ca.volume: "cacerts" is not a volume`},
		{"publish ca without a CA mode", []edit{{sp, "tls_modes: [acme, dns-cloudflare, byo, internal, none]", "tls_modes: [acme, dns-cloudflare, none]"}}, nil, "front.publish_ca: no mode offered has a CA to publish"},
		{"advanced with default", []edit{{sp, `prompt: "Log level (debug, info, warning)", advanced: true`, `prompt: "Log level (debug, info, warning)", default: info, advanced: true`}}, nil, "inputs[9].advanced: an advanced input is never asked"},
		{"advanced without compose default", []edit{{cp, "${LOG_LEVEL:-info}", "${LOG_LEVEL}"}}, nil, "${LOG_LEVEL} is an advanced input, recorded only when given"},
		{"rotation unread", []edit{{cp, "      INTERNAL_SERVICE_SECRET_PREVIOUS: ${INTERNAL_SERVICE_SECRET_PREVIOUS}\n", ""}}, nil, "secrets[2].rotation: deploy/compose.yaml reads no ${INTERNAL_SERVICE_SECRET_PREVIOUS}"},
		{"rotation name taken", []edit{{sp, "  - { key: LOG_LEVEL, ", "  - { key: INTERNAL_SERVICE_SECRET_NEXT, prompt: x }\n  - { key: LOG_LEVEL, "}}, nil, "INTERNAL_SERVICE_SECRET_NEXT is already declared as a rotated secret's next value"},
		{"duplicate route", []edit{{sp, "- name: ingest", "- name: api"}}, nil, "duplicate route"},
		{"bad domain input", []edit{{sp, "domain_input: INGEST_DOMAIN", "domain_input: ingest-domain"}}, nil, "not an environment variable name"},
		{"bad upstream", []edit{{sp, "upstream: ingest:8080", "upstream: ingest"}}, nil, "must be <service>:<port>"},
		{"bad header", []edit{{sp, "strip_headers: [X-Auth-Request-*, X-Atlas-Tenant]\n    - name: api", "strip_headers: [\"X Bad\"]\n    - name: api"}}, nil, "not a header name"},
		{"loopback names no route", []edit{{sp, "ingest: 8080 }", "ingest: 8080, admin: 8081 }"}}, nil, "loopback_ports.admin"},
		{"loopback bad port", []edit{{sp, "ingest: 8080 }", "ingest: 80800 }"}}, nil, "80800 is not a TCP port"},
		{"health unknown provider", []edit{{sp, "provider: http-json", "provider: grpc"}}, nil, "health: unknown provider"},
		{"health no expect", []edit{{sp, "  expect: { status: ready }\n", ""}}, nil, "expect is required"},
		{"health bad port", []edit{{sp, "  port: 9090\n  path: /ready", "  port: 0\n  path: /ready"}}, nil, "is not a TCP port"},
		{"engine", []edit{{sp, "engine: postgres", "engine: mysql"}}, nil, "only postgres"},
		{"database name", []edit{{sp, "  name: atlas\n  roles", "  roles"}}, nil, "database.name"},
		{"database roles", []edit{{sp, "roles: { migrate: atlas, app: atlas_app }", "roles: { migrate: atlas }"}}, nil, "migrate and app"},
		{"database service", []edit{{sp, "  service: postgres\n  name: atlas", "  name: atlas"}}, nil, "database.service: is required"},
		{"barrier without $1", []edit{{sp, "WHERE version > $1 AND", "WHERE version > 0 AND"}}, nil, "$1"},
		{"sql without current", []edit{{sp, "    current: \"SELECT max(version) FROM schema_migrations WHERE success\"\n", ""}}, nil, "current is required"},
		{"negative uid", []edit{{sp, "spool:    { uid: 65532 }", "spool:    { uid: -1 }"}}, nil, "uid must be >= 0"},
		{"reset keeps", []edit{{sp, "reset: [postgres, spool]", "reset: [postgres, certs]"}}, nil, "keep_on_reset"},
		{"secret key name", []edit{{sp, "key: APP_DB_PASSWORD", "key: app-db"}}, nil, "not an environment variable name"},
		{"input without prompt", []edit{{sp, `prompt: "Organisation name",        required: true`, `required: true`}}, nil, "prompt: is required"},
		{"required and optional", []edit{{sp, `prompt: "Organisation name",        required: true`, `prompt: "x", required: true, optional: true`}}, nil, "exclusive"},
		{"extension bad name", []edit{{sp, "name: atlas-search,", "name: Atlas_Search,"}}, nil, `extensions[0].name: "Atlas_Search" must be an app name`},
		{"extension is the project", []edit{{sp, "name: atlas-search,", "name: atlas,"}}, nil, `extensions[0].name: "atlas" is this project`},
		{"extension twice", []edit{{sp, "  - { name: atlas-search, input: SEARCH_URL, key_input: SEARCH_KEY, prompt: \"Full-text search over the events\" }\n", "  - { name: atlas-search, input: SEARCH_URL, key_input: SEARCH_KEY, prompt: \"Full-text search over the events\" }\n  - { name: atlas-search, input: LOG_LEVEL, prompt: x }\n"}}, nil, `extensions[1].name: duplicate extension "atlas-search"`},
		{"extension without prompt", []edit{{sp, `key_input: SEARCH_KEY, prompt: "Full-text search over the events" }`, `key_input: SEARCH_KEY }`}}, nil, "extensions[0].prompt: is required"},
		{"extension without input", []edit{{sp, "input: SEARCH_URL, key_input", "key_input"}}, nil, "extensions[0].input: is required"},
		{"extension input unknown", []edit{{sp, "input: SEARCH_URL, key_input", "input: SEARCH_ADDR, key_input"}}, nil, `extensions[0].input: "SEARCH_ADDR" is not an input`},
		{"extension input not optional", []edit{{sp, `prompt: "Search service URL (empty: no search)", optional: true`, `prompt: "Search service URL (empty: no search)"`}}, nil, "extensions[0].input: SEARCH_URL must be an optional, non-secret input"},
		{"extension input secret", []edit{{sp, `prompt: "Search service URL (empty: no search)", optional: true`, `prompt: "Search service URL (empty: no search)", optional: true, secret: true`}}, nil, "extensions[0].input: SEARCH_URL must be an optional, non-secret input"},
		{"extension input unread", []edit{{cp, "      SEARCH_URL: ${SEARCH_URL:-}\n", ""}}, nil, "extensions[0].input: SEARCH_URL is read by no compose service and no hook"},
		{"extension key unknown", []edit{{sp, "key_input: SEARCH_KEY,", "key_input: SEARCH_TOKEN,"}}, nil, `extensions[0].key_input: "SEARCH_TOKEN" is not an input`},
		{"extension key not secret", []edit{{sp, `prompt: "Search service key",       secret: true, optional: true`, `prompt: "Search service key", optional: true`}}, nil, "extensions[0].key_input: SEARCH_KEY must be an optional secret input"},
		{"extension key not optional", []edit{{sp, `prompt: "Search service key",       secret: true, optional: true`, `prompt: "Search service key", secret: true`}}, nil, "extensions[0].key_input: SEARCH_KEY must be an optional secret input"},
		{"extension input taken", []edit{{sp, "key_input: SEARCH_KEY,", "key_input: SEARCH_URL,"}}, nil, "extensions[0].key_input: SEARCH_URL is already the input of the extension atlas-search"},
		{"extension input advanced", []edit{{sp, `prompt: "Search service URL (empty: no search)", optional: true`, `prompt: "Search service URL (empty: no search)", optional: true, advanced: true`}}, nil, "extensions[0].input: SEARCH_URL must be asked on every install (no advanced, no when)"},
		{"extension input in one mode", []edit{{sp, `prompt: "Search service URL (empty: no search)", optional: true`, `prompt: "Search service URL (empty: no search)", optional: true, when: tls`}}, nil, "extensions[0].input: SEARCH_URL must be asked on every install (no advanced, no when)"},
		{"extension key on other shapes", []edit{{sp, `prompt: "Search service key",       secret: true, optional: true`, `prompt: "Search service key", secret: true, optional: true, shapes: [docker]`}}, nil, "extensions[0].key_input: SEARCH_KEY must be asked wherever SEARCH_URL is"},
		{"extension key advanced", []edit{{sp, `prompt: "Search service key",       secret: true, optional: true`, `prompt: "Search service key", secret: true, optional: true, advanced: true`}}, nil, "extensions[0].key_input: SEARCH_KEY must be asked wherever SEARCH_URL is"},
		{"extension input with a default", []edit{{sp, `prompt: "Search service URL (empty: no search)", optional: true`, `prompt: "Search service URL (empty: no search)", optional: true, default: "http://search:7700"`}}, nil, "extensions[0].input: SEARCH_URL must have no default"},
		{"extension key unread", []edit{{cp, "      SEARCH_KEY: ${SEARCH_KEY:-}\n", ""}}, nil, "extensions[0].key_input: SEARCH_KEY is read by no compose service and no hook"},
		{"service with a profile", []edit{{cp, "      SUPPORT_EMAIL: ${SUPPORT_EMAIL:-}\n", "      SUPPORT_EMAIL: ${SUPPORT_EMAIL:-}\n    profiles: [debug]\n"}}, nil, "services.ui.profiles: nothing turns a profile on: remove `profiles:`"},
		{"endpoint no route", []edit{{sp, "endpoint: api\n", "endpoint: admin\n"}}, nil, `endpoint: "admin" is not a route of front.routes (console, api, ingest)`},
		{"bad when", []edit{{sp, "when: tls,", "when: always,"}}, nil, "expected tls or loopback"},
		{"bad shape", []edit{{sp, "shapes: [docker, kairos] }\n  - { key: ADMIN_EMAIL", "shapes: [docker, nomad] }\n  - { key: ADMIN_EMAIL"}}, nil, "unknown shape"},
		{"unknown image var", []edit{{cp, "ATLAS_VERSION: ${STOWAGE_PROJECT_VERSION}", "ATLAS_VERSION: ${STOWAGE_IMAGE_TYPO}"}}, nil, "STOWAGE_IMAGE_TYPO names no image"},
		{"required var undeclared", []edit{{cp, "${UPSTREAM_API_KEY:-}", "${UPSTREAM_KEY:?set the upstream key}"}}, nil, "${UPSTREAM_KEY} has no default"},
		{"nested var undeclared", []edit{{cp, "${LOG_LEVEL:-info}", "${LOG_LEVEL:-${LOG_FALLBACK}}"}}, nil, "${LOG_FALLBACK} has no default"},
		{"block scalar var", []edit{{cp, "      SUPPORT_EMAIL: ${SUPPORT_EMAIL:-}\n", "      SUPPORT_EMAIL: ${SUPPORT_EMAIL:-}\n    command: |\n      # ${SEARCH_TOKEN}\n      run\n"}}, nil, "${SEARCH_TOKEN} has no default"},
		{"provision cmd", []edit{{sp, "    cmd: [provision.py]\n", ""}}, nil, "cmd is required"},
		{"provision outputs path", []edit{{sp, "outputs: credentials.txt", "outputs: ../credentials.txt"}}, nil, "must be a file name"},
		{"disk pressure order", []edit{{sp, "soft_pct: 75", "soft_pct: 95"}}, nil, "soft_pct < hard_pct"},
		{"disk pressure secret", []edit{{sp, "auth_secret: INTERNAL_SERVICE_SECRET", "auth_secret: NOPE"}}, nil, "is not a secret"},
		{"disk pressure https", []edit{{sp, "post: http://api:9090", "post: https://api:9090"}}, nil, "must be an http:// URL"},
		{"disk pressure host", []edit{{sp, "post: http://api:9090", "post: http://api"}}, nil, "must name <service>:<port>"},
		{"disk pressure service", []edit{{sp, "post: http://api:9090", "post: http://admin:9090"}}, nil, `service "admin"`},
		{"backup default profile", []edit{{sp, "default_profile: full", "default_profile: nightly"}}, nil, "is not a profile"},
		{"backup profile shape", []edit{{sp, "    full: all\n", "    full: everything\n"}}, nil, "must be \"all\", a table list, or { exclude"},
		{"backup exclude pattern", []edit{{sp, "lean: { exclude: [events", "lean: { exclude: [events-x"}}, nil, "is not a table pattern"},
		{"backup profile name", []edit{{sp, "    config: [organisations", "    con-fig: [organisations"}}, nil, "lowercase letters and digits"},
		{"postgres mode", []edit{{sp, "postgres_modes: [embedded, external]", "postgres_modes: [rds]"}}, nil, "unknown mode \"rds\""},
		{"kairos base", []edit{{sp, "    base: ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55\n", ""}}, nil, "shapes.kairos.base: is required"},
		{"kairos init", []edit{{sp, "    kairos_init: quay.io/kairos/kairos-init:v0.16.2@sha256:0777266d25e31b653deaaaa084d4e27d98f7a55e82e42f0648e2ba9c756b8664\n", ""}}, nil, "shapes.kairos.kairos_init: is required"},
		{"kairos base unpinned", []edit{{sp, "base: ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55", "base: ubuntu:24.04"}}, nil, `shapes.kairos.base: "ubuntu:24.04" must be pinned by digest`},
		{"kairos init unpinned", []edit{{sp, "kairos-init:v0.16.2@sha256:0777266d25e31b653deaaaa084d4e27d98f7a55e82e42f0648e2ba9c756b8664", "kairos-init:v0.16.2"}}, nil, `"quay.io/kairos/kairos-init:v0.16.2" must be pinned by digest`},
		{"pull shares the push token", []edit{{sp, "token_env: STOWAGE_PULL_TOKEN", "token_env: STOWAGE_PUSH_TOKEN"}}, nil, "registry.pull: token_env STOWAGE_PUSH_TOKEN is registry.push's"},
		{"pull is the push docker config", []edit{{sp, "pull: { provider: token, user: atlas-pull, token_env: STOWAGE_PULL_TOKEN }", "pull: { provider: docker-config, config: ci }"}, {sp, "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }", "push: { provider: docker-config, config: ci/ }"}}, nil, "registry.pull: is registry.push's Docker config"},
		{"cloud", []edit{{sp, "cloud_publish: [aws, azure, gcp]", "cloud_publish: [ovh]"}}, nil, "unknown cloud"},
		{"host api service", []edit{{sp, "host_api: [api]", "host_api: [admin]"}}, nil, `shapes.kairos.host_api: service "admin" is not in`},
		{"host api twice", []edit{{sp, "host_api: [api]", "host_api: [api, api]"}}, nil, `service "api" is named twice`},
		{"health gate", []edit{{sp, "poll_s: 30 }", "poll_s: 900 }"}}, nil, "poll_s < deadline_s"},
		{"lock not semver", nil, map[string]string{"stowage.lock": "stowage: one\n"}, "stowage: version \"one\" is not MAJOR.MINOR.PATCH"},
		{"lock unknown key", nil, map[string]string{"stowage.lock": "stowage: 1.0.0\nruntimes: {}\n"}, "runtimes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := testutil.CopyExample(t, "full")
			for _, e := range c.edits {
				testutil.Edit(t, root, e.file, e.old, e.new)
			}
			for f, content := range c.write {
				testutil.Write(t, root, f, content)
			}
			res, err := Run(root, filepath.Join(root, specFile), now)
			var errs []string
			if err != nil {
				errs = []string{err.Error()} // strict decoding refuses before any rule runs
			} else {
				errs = errorsOf(res)
			}
			for _, e := range errs {
				if strings.Contains(e, c.want) {
					return
				}
			}
			t.Fatalf("no error containing %q; got:\n%s", c.want, strings.Join(errs, "\n"))
		})
	}
}

// Rules of a project without a database: what only a database needs is refused where it is
// declared. examples/stateless being clean is the positive control.
func TestEachRuleFiresOnStateless(t *testing.T) {
	for _, c := range []struct{ name, add, want string }{
		{"backup", "backup:\n  provider: pg-age-rclone\n  profiles: { full: all }\n", "backup: a backup dumps the database, and the project declares none"},
		{"data reset", "data:\n  volumes:\n    cache: { uid: 0 }\n  reset: [cache]\n", "data.reset: reset-db resets a database, and the project declares none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := testutil.CopyExample(t, "stateless")
			testutil.Edit(t, root, specFile, "\nshapes:\n", "\n"+c.add+"\nshapes:\n")
			errs := strings.Join(errorsOf(run(t, root)), "\n")
			if !strings.Contains(errs, c.want) {
				t.Fatalf("no %q in:\n%s", c.want, errs)
			}
		})
	}
	root := testutil.CopyExample(t, "stateless")
	testutil.Edit(t, root, specFile, "kubernetes: { chart: deploy/chart }", "kubernetes: { chart: deploy/chart, postgres_modes: [external] }")
	if errs := strings.Join(errorsOf(run(t, root)), "\n"); !strings.Contains(errs, "shapes.kubernetes.postgres_modes: the database's modes, and the project declares no database") {
		t.Errorf("postgres_modes: %s", errs)
	}
	// Volumes alone are a stateless project's own: no finding.
	root = testutil.CopyExample(t, "stateless")
	testutil.Edit(t, root, specFile, "\nshapes:\n", "\ndata:\n  volumes:\n    cache: { uid: 0 }\n\nshapes:\n")
	if res := run(t, root); len(res.Findings) != 0 {
		t.Errorf("findings: %v", res.Findings)
	}
}

// Rules about an emptied section, set through the parsed spec so no other text is disturbed.
func TestEmptySections(t *testing.T) {
	cases := []struct {
		path  []string
		value any
		want  string
	}{
		{[]string{"images"}, map[string]any{}, "at least one image"},
		{[]string{"front", "routes"}, []any{}, "at least one route"},
		{[]string{"shapes"}, map[string]any{}, "at least one deploy shape"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.path, "."), func(t *testing.T) {
			root := testutil.CopyExample(t, "full")
			testutil.SetYAML(t, root, specFile, c.path, c.value)
			errs := strings.Join(errorsOf(run(t, root)), "\n")
			if !strings.Contains(errs, c.want) {
				t.Fatalf("no %q in:\n%s", c.want, errs)
			}
		})
	}
}

// The revision-suffix rule is reached through the version: a SemVer pre-release spelled like a
// revision cannot ship, its neighbour can.
func TestRevisionLikeVersionRefused(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	testutil.Write(t, root, "VERSION", "1.1.0-r2\n")
	testutil.Write(t, root, "changes/1.1.0-r2.md", "- x\n")
	found := errorsOf(run(t, root))
	if len(found) != 1 || !strings.Contains(found[0], "version.scheme") || !strings.Contains(found[0], "ends like a revision suffix") {
		t.Fatalf("want one version.scheme error, got:\n%s", strings.Join(found, "\n"))
	}
	testutil.Write(t, root, "VERSION", "1.1.0-rc2\n")
	testutil.Write(t, root, "changes/1.1.0-rc2.md", "- x\n")
	if errs := errorsOf(run(t, root)); len(errs) > 0 {
		t.Fatalf("1.1.0-rc2 refused:\n%s", strings.Join(errs, "\n"))
	}
}

// Findings come out in the same order on every run.
func TestFindingsAreDeterministic(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, specFile, "{ console: 3000, api: 9090, ingest: 8080 }", "{ console: 0, api: 70000, ingest: 99999, a: 1, b: 2 }")
	first := run(t, root).Findings
	for range 20 {
		again := run(t, root).Findings
		for i := range first {
			if first[i] != again[i] {
				t.Fatalf("finding %d differs: %v vs %v", i, first[i], again[i])
			}
		}
	}
}

func TestMissingLock(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	if err := os.Remove(filepath.Join(root, "stowage.lock")); err != nil {
		t.Fatal(err)
	}
	res := run(t, root)
	found := false
	for _, f := range res.Findings {
		if f.Severity == Error && f.Where == "stowage.lock" && strings.Contains(f.Message, "run stowage lock") {
			found = true
		}
	}
	if !found {
		t.Error("missing lock not an error")
	}
	// A lock that does not load is reported once under its name, not prefixed twice.
	testutil.Write(t, root, "stowage.lock", "stowage: one\n")
	found = false
	for _, f := range run(t, root).Findings {
		if f.Severity == Error && f.Where == "stowage.lock" {
			found = strings.HasPrefix(f.Message, "stowage: version")
			if !found {
				t.Errorf("lock load error: %s", f)
			}
		}
	}
	if !found {
		t.Error("a lock that does not load is not reported")
	}
	// A valid lock silences it.
	testutil.Write(t, root, "stowage.lock", "stowage: 1.0.0\n")
	for _, f := range run(t, root).Findings {
		if strings.Contains(f.Where, "stowage.lock") {
			t.Errorf("valid lock still reported: %s", f)
		}
	}
}

func TestStrictSpecDecoding(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, specFile, "\nname: notes\n", "\nname: notes\nnmae: typo\n")
	if _, err := Run(root, filepath.Join(root, specFile), now); err == nil || !strings.Contains(err.Error(), "nmae") {
		t.Fatalf("unknown key accepted: %v", err)
	}
	testutil.Edit(t, root, specFile, "apiVersion: stowage/v1", "apiVersion: stowage/v2")
	if _, err := Run(root, filepath.Join(root, specFile), now); err == nil || !strings.Contains(err.Error(), "upgrade stowage") {
		t.Fatalf("future apiVersion accepted: %v", err)
	}
}

func TestComposeVarParsing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	content := "services:\n  a:\n    image: ${STOWAGE_IMAGE_A}\n    command: echo $$HOME $PLAIN ${D:-x} # ${COMMENTED}\n    entrypoint: |\n      # ${IN_BLOCK}\n      run\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := ParseCompose(p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]VarRef{}
	for _, v := range c.Vars {
		got[v.Name] = v
	}
	if _, ok := got["HOME"]; ok {
		t.Error("$$ escape read as a variable")
	}
	if _, ok := got["COMMENTED"]; ok {
		t.Error("a YAML comment was read")
	}
	if v, ok := got["IN_BLOCK"]; !ok || v.Line != 5 {
		t.Errorf("a '#' line inside a block scalar is compose text: %+v", v)
	}
	if v := got["PLAIN"]; v.HasDefault || v.Line != 4 {
		t.Errorf("PLAIN = %+v", v)
	}
	if v := got["D"]; !v.HasDefault || v.Default != "x" || v.Op != ":-" {
		t.Errorf("D = %+v", v)
	}
}

func TestImageVar(t *testing.T) {
	if got := ImageVar("admin-api"); got != "STOWAGE_IMAGE_ADMIN_API" {
		t.Errorf("ImageVar = %s", got)
	}
}

func TestInterpolations(t *testing.T) {
	cases := []struct {
		in   string
		want []string // Name/Op/Default triples, flattened
	}{
		{"plain", nil},
		{"$$HOME and $$", nil},
		{"$A-b", []string{"A", "", ""}},
		{"${A}", []string{"A", "", ""}},
		{"${A:-x}${B-y}", []string{"A", ":-", "x", "B", "-", "y"}},
		{"${A:-${B:-z}}", []string{"A", ":-", "${B:-z}", "B", ":-", "z"}},
		{"${A:?need ${B}}", []string{"A", ":?", "need ${B}", "B", "", ""}},
		{"${A:-$$}", []string{"A", ":-", "$$"}},
		{"$1 and $", nil},
	}
	for _, c := range cases {
		refs, err := Interpolations(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		var got []string
		for _, r := range refs {
			got = append(got, r.Name, r.Op, r.Default)
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"${A", "${}", "${1A}", "${A!x}", "${A:-${B}"} {
		if _, err := Interpolations(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Properties: parsing never panics, every reference names a valid variable, and a value with no
// '$' has no reference.
func FuzzInterpolations(f *testing.F) {
	for _, s := range []string{"${A:-${B}}", "$$A", "${A", "x $B y", "${A:?${B:+${C}}}"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		refs, err := Interpolations(s)
		if err != nil {
			return
		}
		if !strings.Contains(s, "$") && len(refs) > 0 {
			t.Fatalf("%q: references without a dollar", s)
		}
		for _, r := range refs {
			if r.Name == "" || !isNameStart(r.Name[0]) {
				t.Fatalf("%q: bad name %q", s, r.Name)
			}
		}
	})
}

// `${X:+v}` and `${X+v}` are defined when X is unset: they need no declaration (positive control
// for the required-variable rule).
func TestAlternateValueNeedsNoDeclaration(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "${LOG_LEVEL:-info}", "info${DEBUG:+-debug}${TRACE+-trace}")
	if errs := errorsOf(run(t, root)); len(errs) > 0 {
		t.Fatalf("errors:\n%s", strings.Join(errs, "\n"))
	}
}

func TestGitInBuildContext(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	testutil.Write(t, root, ".git/HEAD", "ref: refs/heads/main\n")
	errs := strings.Join(errorsOf(run(t, root)), "\n")
	for _, img := range []string{"images.api.context", "images.ingest.context"} {
		if !strings.Contains(errs, img+": .git: ") {
			t.Errorf("no .git error for %s:\n%s", img, errs)
		}
	}
	if strings.Contains(errs, "images.ui.context") {
		t.Error("ui's context (ui/) holds no .git")
	}
	testutil.Write(t, root, ".dockerignore", ".git\n")
	if errs := errorsOf(run(t, root)); len(errs) > 0 {
		t.Fatalf("excluded .git still reported:\n%s", strings.Join(errs, "\n"))
	}
}

// A scheme that cannot answer is reported once, against the scheme.
func TestFailingSchemeReportedOnce(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	testutil.Write(t, root, "scheme.sh", "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(root, "scheme.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Edit(t, root, specFile, "scheme: { provider: semver }", "scheme: { provider: command, cmd: [./scheme.sh] }")
	found := errorsOf(run(t, root))
	if len(found) != 1 || !strings.Contains(found[0], "version.scheme") {
		t.Fatalf("want one version.scheme error, got:\n%s", strings.Join(found, "\n"))
	}
}

// A VM backup by another provider than pg-age-rclone is warned: the VM schedules none.
func TestVMBackupCommandWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, specFile, "  provider: pg-age-rclone\n  profiles: { full: all }\n", "  provider: command\n  image: api\n  cmd: [notes-api, backup]\n")
	res := run(t, root)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("errors:\n%s", strings.Join(errs, "\n"))
	}
	if len(res.Findings) != 1 || !strings.Contains(res.Findings[0].Message, "the VM runs pg-age-rclone backups only") {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// An input counts as read when the provision hook's env lists it, and only then: a hook no longer
// receives every input.
func TestInputOnlyTheHookReads(t *testing.T) {
	t.Parallel()
	warned := func(res *Result) bool {
		for _, f := range res.Findings {
			if f.Severity == Warning && strings.Contains(f.Message, "ADMIN_EMAIL is asked but read by no compose service and no hook") {
				return true
			}
		}
		return false
	}
	root := testutil.CopyExample(t, "full")
	res := run(t, root)
	if warned(res) {
		t.Fatal("warned while the hook's env lists it")
	}
	// An input asked on Kubernetes only is the chart's to read, never the compose file's.
	for _, f := range res.Findings {
		if strings.Contains(f.Message, "DATABASE_URL is asked") {
			t.Errorf("warned: %s", f.Message)
		}
	}
	testutil.Edit(t, root, specFile, "env: [ADMIN_EMAIL, ADMIN_PASSWORD,", "env: [ADMIN_PASSWORD,")
	if res := run(t, root); !warned(res) {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// A disk-pressure hook with no kairos shape is warned: only the VM's host daemon acts on it.
func TestDiskPressureWithoutKairosWarns(t *testing.T) {
	t.Parallel()
	warned := func(res *Result) bool {
		for _, f := range res.Findings {
			if f.Where == "hooks.maintenance.disk_pressure" && strings.Contains(f.Message, "only the kairos shape's host daemon watches the disk") {
				return true
			}
		}
		return false
	}
	if warned(run(t, testutil.CopyExample(t, "full"))) {
		t.Fatal("warned with a kairos shape")
	}
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, specFile, "    env: [ADMIN_EMAIL]\n", "    env: [ADMIN_EMAIL]\n  maintenance:\n    disk_pressure: { soft_pct: 75, hard_pct: 90, post: \"http://api:8000/purge\" }\n")
	if warned(run(t, root)) {
		t.Fatal("warned with a kairos shape")
	}
	// The kairos shape removed: from its key to the end of the spec.
	raw, err := os.ReadFile(filepath.Join(root, specFile))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(raw), "  kairos:\n")
	if i < 0 {
		t.Fatal("no kairos shape in the minimal example")
	}
	testutil.Edit(t, root, specFile, string(raw[i:]), "")
	if res := run(t, root); !warned(res) {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// A chart that never reads the release's images is warned: its pods would not run them by digest.
func TestChartNotReadingTheImagesWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, "deploy/chart/templates/_helpers.tpl", "index .root.Values.stowage.images .key", "index .root.Values.images .key")
	res := run(t, root)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("errors:\n%s", strings.Join(errs, "\n"))
	}
	if len(res.Findings) != 1 || !strings.Contains(res.Findings[0].Message, "no template reads .Values.stowage.images") {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// A chart that never reads the Secret `<short>-stow k8s` writes is warned while the release has a
// secret or an input on Kubernetes, and not once it has none.
func TestChartNotReadingTheSecretWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, "deploy/chart/templates/web.yaml", "with .Values.stowage.secret", "with .Values.stowage.secretName")
	res := run(t, root)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("errors:\n%s", strings.Join(errs, "\n"))
	}
	if len(res.Findings) != 1 || !strings.Contains(res.Findings[0].Message, "no template reads .Values.stowage.secret") {
		t.Fatalf("findings: %+v", res.Findings)
	}
	testutil.Edit(t, root, specFile, "  - { key: SESSION_SECRET,    gen: hex32 }\n  - { key: POSTGRES_PASSWORD, gen: pass18 }\n", "")
	testutil.Edit(t, root, specFile, `  - { key: ADMIN_EMAIL, prompt: "Administrator email", required: true }`, `  - { key: ADMIN_EMAIL, prompt: "Administrator email", required: true, shapes: [docker] }`)
	for _, f := range run(t, root).Findings {
		if strings.Contains(f.Message, ".Values.stowage.secret") {
			t.Errorf("warned with nothing to read: %s", f.Message)
		}
	}
}

// A chart of a project whose images need the pull account, that never reads the pull Secret, is
// warned.
func TestChartNotReadingThePullSecretWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, "deploy/chart/templates/_helpers.tpl", "with .Values.stowage.pullSecret", "with .Values.pullSecret")
	res := run(t, root)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("errors:\n%s", strings.Join(errs, "\n"))
	}
	if len(res.Findings) != 1 || !strings.Contains(res.Findings[0].Message, "no template reads .Values.stowage.pullSecret") {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

func TestCustomFrontendWarns(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, "provision/Dockerfile", "FROM python", "# syntax=registry.example.com/frontend:1\nFROM python")
	res := run(t, root)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("errors:\n%s", strings.Join(errs, "\n"))
	}
	warned := false
	for _, f := range res.Findings {
		warned = warned || strings.Contains(f.Message, "cannot be checked")
	}
	if !warned {
		t.Error("custom frontend not warned")
	}
}

func TestChannelNameIsATag(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"stable": true, "lts.2": true, "release_candidate": true, "1-x": true,
		strings.Repeat("a", 128): true, strings.Repeat("a", 129): false,
		"": false, "Beta": false, "-x": false, "x-": false, "a..b": false, "a/b": false,
	} {
		if ChannelName(name) != want {
			t.Errorf("%q: %v", name, !want)
		}
	}
}
