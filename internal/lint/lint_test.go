package lint

import (
	"net"
	"os"
	"path/filepath"
	"slices"
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
		{"k8s schema without sql", []edit{{specFile, "kubernetes: { chart: deploy/chart, postgres_modes: [external, cnpg] }", "kubernetes: { chart: deploy/chart, postgres_modes: [external, cnpg], schema: { url_env: DATABASE_URL, image: postgres } }"}}, nil, "shapes.kubernetes.schema: needs a database whose schema_compat provider is sql"},
		{"bad requires", []edit{{specFile, `requires: ">=0.1 <1"`, `requires: "~0.1"`}}, nil, "stowage.requires: \"~0.1\": a comparator starts with"},
		{"lock outside requires", []edit{{specFile, `requires: ">=0.1 <1"`, `requires: ">0.1"`}}, nil, "pins stowage 0.1.0, which stowage.requires (>0.1) does not accept"},
		{"bad short name", []edit{{specFile, "\nname: notes\n", "\nname: notes\nshort_name: No\n"}}, nil, "short_name:"},
		{"stowage short name", []edit{{specFile, "\nname: notes\n", "\nname: notes\nshort_name: stowage-notes\n"}}, nil, "keeps for Stowage's own files"},
		{"compose build", []edit{{composeFile, "image: ${STOWAGE_IMAGE_WEB}", "image: ${STOWAGE_IMAGE_WEB}\n    build: ./web"}}, nil, "build: is forbidden"},
		{"compose name", []edit{{composeFile, "services:", "name: notes\nservices:"}}, nil, "top-level name"},
		{"extends another file", []edit{{composeFile, "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n", "  web:\n    extends: { file: base.yaml, service: api }\n"}}, nil, "services.web.extends: file: names another compose file"},
		{"extends unknown service", []edit{{composeFile, "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n", "  web:\n    extends: www\n"}}, nil, `services.web.extends: extends "www", which is not a service`},
		{"extends loop", []edit{{composeFile, "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n", "  web:\n    extends: { service: web }\n"}}, nil, "extends loops: web → web"},
		{"extends loop reached from outside", []edit{{composeFile, "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n", "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n  x:\n    extends: y\n  y:\n    extends: x\n  a:\n    extends: x\n"}}, nil, "services.x.extends: extends loops: x → y → x"},
		{"extends joins the base's ports", []edit{{composeFile, "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n", "  base:\n    image: ${STOWAGE_IMAGE_WEB}\n    ports: [\"127.0.0.1:1:1\", \"5432:5432\"]\n  web:\n    extends: base\n    image: ${STOWAGE_IMAGE_WEB}\n    ports: [\"127.0.0.1:9000:9000\"]\n"}}, nil, "services.web.ports: the VM's firewall"},
		{"extends inherits a literal image", []edit{{composeFile, "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n", "  base:\n    image: nginx:latest\n  web:\n    extends: base\n"}}, nil, "services.web.image"},
		{"literal image", []edit{{composeFile, "image: ${STOWAGE_IMAGE_WEB}", "image: nginx:latest"}}, nil, "single deployment point"},
		{"image var of unknown key", []edit{{composeFile, "image: ${STOWAGE_IMAGE_WEB}", "image: ${STOWAGE_IMAGE_NOPE}"}}, nil, "for a key of images"},
		{"safe default relaxed", []edit{{composeFile, "${SECURE_COOKIES:-true}", "${SECURE_COOKIES:-false}"}}, nil, "${SECURE_COOKIES:-true}"},
		{"safe default without default", []edit{{composeFile, "${SECURE_COOKIES:-true}", "${SECURE_COOKIES}"}}, nil, "${SECURE_COOKIES:-true}"},
		{"safe default unused", []edit{{composeFile, "      SECURE_COOKIES: ${SECURE_COOKIES:-true}\n", ""}}, nil, "not used in"},
		{"undeclared variable", []edit{{composeFile, "${LOG_LEVEL:-info}", "${LOG_LEVEL}"}}, nil, "${LOG_LEVEL} has no default"},
		{"unknown STOWAGE_ var", []edit{{composeFile, "${LOG_LEVEL:-info}", "${STOWAGE_NOPE:-x}"}}, nil, "not a variable Stowage provides"},
		{"reserved image key", []edit{{specFile, "  web: { context: web", "  release: { context: web"}, {composeFile, "${STOWAGE_IMAGE_WEB}", "${STOWAGE_IMAGE_RELEASE}"}}, nil, "is reserved"},
		{"backup image key", []edit{{specFile, "  web: { context: web", "  backup: { context: web"}, {composeFile, "${STOWAGE_IMAGE_WEB}", "${STOWAGE_IMAGE_BACKUP}"}}, nil, `"backup" is reserved`},
		{"chown image key", []edit{{specFile, "  web: { context: web", "  chown: { context: web"}, {composeFile, "${STOWAGE_IMAGE_WEB}", "${STOWAGE_IMAGE_CHOWN}"}}, nil, `"chown" is reserved`},
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
		{"memory share of 0", []edit{{sp, memShares, "shares: { postgres: 0 }"}}, nil, "stack.memory.shares.postgres: 0: a percent of the host's RAM, from 1 to 90"},
		{"memory share over 90", []edit{{sp, memShares, "shares: { postgres: 91 }"}}, nil, "stack.memory.shares.postgres: 91: a percent of the host's RAM, from 1 to 90"},
		{"memory shares over 90", []edit{{sp, memShares, "shares: { postgres: 60, api: 31 }"}}, nil, "stack.memory.shares: the shares total 91 %: at most 90"},
		{"memory share of no service", []edit{{sp, memShares, "shares: { postgres: 25, search: 10 }"}}, nil, `stack.memory.shares.search: service "search" is not in deploy/compose.yaml`},
		{"memory share max under the least", []edit{{sp, memShares, "shares: { api: { percent: 25, max: 67108863 } }"}}, nil, "stack.memory.shares.api.max: 67108863: 67108863 for api, under 64M, the least limit"},
		{"memory share max under the least per container", []edit{{sp, memShares, "shares: { api: { percent: 10, max: 255M } }"}, {"deploy/compose.yaml", "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    deploy: { replicas: \"4\" }\n    image: ${STOWAGE_IMAGE_API}\n"}}, nil, "stack.memory.shares.api.max: 255M: 65280K for each of api's 4 containers, under 64M"},
		{"memory share max under a GiB reservation", []edit{{sp, memShares, "shares: { postgres: { percent: 25, max: 1535M } }"}, {"deploy/compose.yaml", "    mem_limit: 256m ", "    mem_reservation: 1.5GiB\n    mem_limit: 256m "}}, nil, "stack.memory.shares.postgres.max: 1535M: 1535M for postgres, under the 1536M limit"},
		{"memory share max under the compose limit", []edit{{sp, memShares, "shares: { postgres: { percent: 25, max: 255M } }"}}, nil, "stack.memory.shares.postgres.max: 255M: 255M for postgres, under the 256M limit or reservation deploy/compose.yaml gives it itself"},
		{"memory share max under the replicas' limits", []edit{{sp, memShares, "shares: { postgres: { percent: 25, max: 511M } }"}, {"deploy/compose.yaml", "    mem_limit: 256m ", "    scale: 2\n    mem_limit: 256m "}}, nil, "stack.memory.shares.postgres.max: 511M: 261632K for each of postgres's 2 containers, under the 256M limit or reservation"},
		{"memory share max under the reservation", []edit{{sp, memShares, "shares: { postgres: { percent: 25, max: 1G } }"}, {"deploy/compose.yaml", "    mem_limit: 256m ", "    mem_reservation: 1.5g\n    mem_limit: 256m "}}, nil, "stack.memory.shares.postgres.max: 1G: 1G for postgres, under the 1536M limit"},
		{"memory share max under the base's limit", []edit{{sp, memShares, "shares: { api: { percent: 25, max: 1G } }"}, {"deploy/compose.yaml", "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    extends: postgres\n    image: ${STOWAGE_IMAGE_API}\n"}, {"deploy/compose.yaml", "    mem_limit: 256m ", "    mem_limit: 2g "}}, nil, "stack.memory.shares.api.max: 1G: 1G for api, under the 2G limit"},
		{"memory share max without percent", []edit{{sp, memShares, "shares: { postgres: { max: 16G } }"}}, nil, "stack.memory.shares.postgres: 0: a percent of the host's RAM"},
		{"vm size empty", []edit{{sp, vmSizeLine, "size: {}"}}, nil, "shapes.kairos.size: states no size"},
		{"vm size states nothing", []edit{{sp, vmSizeLine, "size: { min: {} }"}}, nil, "shapes.kairos.size.min: states nothing"},
		{"vm size negative cpus", []edit{{sp, vmSizeLine, "size: { min: { cpus: -1 } }"}}, nil, "cpus: -1: at least 1"},
		{"vm size small memory", []edit{{sp, vmSizeLine, "size: { min: { memory: 1023M } }"}}, nil, "shapes.kairos.size.min.memory: 1023M: whole MiB, at least 1G"},
		{"vm size partial MiB", []edit{{sp, vmSizeLine, "size: { min: { memory: 2097153K } }"}}, nil, "shapes.kairos.size.min.memory: 2097153K: whole MiB"},
		{"vm size small disk", []edit{{sp, vmSizeLine, "size: { recommended: { disk: 15G } }"}}, nil, "shapes.kairos.size.recommended.disk: 15G: whole GiB, at least 16G"},
		{"vm size partial GiB", []edit{{sp, vmSizeLine, "size: { recommended: { disk: 16385M } }"}}, nil, "shapes.kairos.size.recommended.disk: 16385M: whole GiB"},
		{"vm size min over recommended cpus", []edit{{sp, vmSizeLine, "size: { min: { cpus: 4 }, recommended: { cpus: 3 } }"}}, nil, "shapes.kairos.size.recommended.cpus: 3: under the minimum 4"},
		{"vm size min over recommended memory", []edit{{sp, vmSizeLine, "size: { min: { memory: 8G }, recommended: { memory: 8191M } }"}}, nil, "shapes.kairos.size.recommended.memory: 8191M: under the minimum 8G"},
		{"vm size min over recommended disk", []edit{{sp, vmSizeLine, "size: { min: { disk: 41G }, recommended: { disk: 40G } }"}}, nil, "shapes.kairos.size.recommended.disk: 40G: under the minimum 41G"},
		{"vm size min left empty", []edit{{sp, vmSizeLine, "size: { min: , recommended: { cpus: 2 } }"}}, nil, "shapes.kairos.size.min: states nothing"},
		{"vm size left empty", []edit{{sp, vmSizeLine, "size:"}}, nil, "shapes.kairos.size: states no size"},
		{"memory shares empty", []edit{{sp, memShares, "shares: {}"}}, nil, "stack.memory.shares: names no service"},
		{"memory left empty", []edit{{sp, "  memory:\n    " + memShares + "   # percent of the host's RAM, recomputed at each start; postgres's capped at 16 GiB\n", "  memory:\n"}}, nil, "stack.memory.shares: names no service"},
		{"tune profile unknown", []edit{{sp, "  profile: network   #", "  profile: shared   #"}}, nil, `tune.profile: "shared": default or network`},
		{"tune profile missing", []edit{{sp, "  profile: network   #", "  other: network   #"}}, nil, "field other not found"},
		{"tune profile empty", []edit{{sp, "  profile: network   #", "  profile: \"\"   #"}}, nil, "tune.profile: missing"},
		{"tune left empty", []edit{{sp, "tune:\n  profile: network   # a heavy network load: the hosts are tuned for it (default: Docker's path alone)\n", "tune:\n"}}, nil, "tune.profile: missing"},
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
		{"delete provider unknown", []edit{{sp, "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }", "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }\n  delete: { provider: s3 }"}}, nil, `registry.delete: unknown provider "s3" (oci, github)`},
		{"github delete api not https", []edit{{sp, "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }", "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }\n  delete: { provider: github, api: \"http://api.github.com\" }"}}, nil, `registry.delete: github: api "http://api.github.com" is not an https URL`},
		{"github delete token not a name", []edit{{sp, "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }", "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }\n  delete: { provider: github, token_env: \"a-b\" }"}}, nil, `registry.delete: github: token_env "a-b" is not an environment variable name`},
		{"k8s schema url_env", []edit{{sp, "schema: { url_env: DATABASE_URL, image: postgres }", "schema: { url_env: \"x y\", image: postgres }"}}, nil, `shapes.kubernetes.schema.url_env: "x y" is not an environment variable name`},
		{"k8s schema without reads_newer_image", []edit{{sp, "    reads_newer_label: com.example.atlas.reads-newer-schema   # \"true\" on an image that reads a newer schema\n    reads_newer_image: api                                    # the image that carries it\n", ""}}, nil, "shapes.kubernetes.schema: needs database.schema_compat.reads_newer_image"},
		{"k8s schema image", []edit{{sp, "schema: { url_env: DATABASE_URL, image: postgres }", "schema: { url_env: DATABASE_URL, image: psql }"}}, nil, `shapes.kubernetes.schema.image: "psql" is not an images key`},
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
		{"rotation unread", []edit{{cp, "      INTERNAL_SERVICE_SECRET_PREVIOUS: ${INTERNAL_SERVICE_SECRET_PREVIOUS}\n", ""}}, nil, "secrets[2].rotation: deploy/compose.yaml reads no ${INTERNAL_SERVICE_SECRET_PREVIOUS} (nor ${INTERNAL_SERVICE_SECRET_ACCEPTED}, either of them)"},
		{"rotation accepted name taken", []edit{{sp, "  - { key: LOG_LEVEL, ", "  - { key: INTERNAL_SERVICE_SECRET_ACCEPTED, prompt: x }\n  - { key: LOG_LEVEL, "}}, nil, "INTERNAL_SERVICE_SECRET_ACCEPTED is already declared as a rotated secret's other accepted value"},
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
		{"negative uid", []edit{{sp, "spool:    { uid: 65532 }", "spool:    { uid: -1 }"}}, nil, "uid must be 0 to 4294967294"},
		{"uid past the range", []edit{{sp, "spool:    { uid: 65532 }", "spool:    { uid: 4294967295 }"}}, nil, "uid must be 0 to 4294967294"},
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
		{"service with a profile", []edit{{cp, "      SUPPORT_EMAIL: ${SUPPORT_EMAIL:-}\n", "      SUPPORT_EMAIL: ${SUPPORT_EMAIL:-}\n    profiles: [debug]\n"}}, nil, "services.ui.profiles: no input turns debug on"},
		{"profile of no service", []edit{{cp, "    profiles: [relay]\n", ""}}, nil, "inputs[12].profile: no service of deploy/compose.yaml has the profile relay"},
		{"profile input required", []edit{{sp, "optional: true, profile: relay", "required: true, profile: relay"}}, nil, "a required input always turns relay on"},
		{"profile name", []edit{{sp, "profile: relay,", "profile: -relay,"}, {cp, "profiles: [relay]", "profiles: [-relay]"}}, nil, `"-relay" is not a compose profile name`},
		{"l4 bad name", []edit{{sp, "name: echo,     protocol", "name: Echo,     protocol"}}, nil, `l4.routes[0].name: "Echo" must be 1 to 15`},
		{"l4 name twice", []edit{{sp, "name: echo-udp, protocol: udp", "name: echo, protocol: udp"}}, nil, `l4.routes[1].name: "echo" is already another L4 route's name`},
		{"l4 name of a front route", []edit{{sp, "name: echo-udp, protocol: udp", "name: api, protocol: udp"}}, nil, `l4.routes[1].name: "api" is already a front route's name`},
		{"l4 name too long", []edit{{sp, "name: echo-udp, protocol: udp", "name: echo-udp-streams, protocol: udp"}}, nil, `"echo-udp-streams" must be 1 to 15`},
		{"l4 name with two hyphens", []edit{{sp, "name: echo-udp, protocol: udp", "name: echo--udp, protocol: udp"}}, nil, `"echo--udp" must be 1 to 15`},
		{"l4 name with an underscore", []edit{{sp, "name: echo-udp, protocol: udp", "name: echo_udp, protocol: udp"}}, nil, `"echo_udp" must be 1 to 15`},
		{"l4 protocol", []edit{{sp, "protocol: udp, port: 17007", "protocol: sctp, port: 17007"}}, nil, `l4.routes[1].protocol: "sctp": expected tcp or udp`},
		{"l4 port range", []edit{{sp, "protocol: udp, port: 17007", "protocol: udp, port: 65536"}}, nil, "l4.routes[1].port: 65536 is not a port"},
		{"l4 port twice", []edit{{sp, "protocol: udp, port: 17007", "protocol: tcp, port: 17007"}}, nil, "l4.routes[1].port: tcp/17007 is route echo's port"},
		{"l4 on the front's port", []edit{{sp, "protocol: tcp, port: 17007", "protocol: tcp, port: 443"}}, nil, "l4.routes[0].port: tcp/443 is"},
		{"l4 on a moved front port", []edit{{sp, "https_port: 443", "https_port: 17007"}}, nil, "tcp/17007 is the front's HTTPS port"},
		{"l4 on acme's port", []edit{{sp, "https_port: 443", "https_port: 8443"}, {sp, "protocol: tcp, port: 17007", "protocol: tcp, port: 443"}}, nil, "tcp/443 is acme's TLS challenge port"},
		{"l4 on a loopback port", []edit{{sp, "protocol: tcp, port: 17007", "protocol: tcp, port: 3000"}}, nil, "tcp/3000 is route console's loopback port"},
		{"front ports equal", []edit{{sp, "https_port: 443", "https_port: 80"}}, nil, "front.https_port: 80 is the HTTP port too"},
		{"front port range", []edit{{sp, "http_port: 80 ", "http_port: 70000 "}}, nil, "front.http_port: 70000 is not a TCP port"},
		{"l4 upstream syntax", []edit{{sp, "upstream: echo:17007, allow", "upstream: echo, allow"}}, nil, `l4.routes[1].upstream: "echo" must be <service>:<port>`},
		{"l4 upstream port", []edit{{sp, "upstream: echo:17007, allow", "upstream: echo:0, allow"}}, nil, "l4.routes[1].upstream: 0 is not a port"},
		{"l4 upstream service", []edit{{sp, "upstream: echo:17007, allow", "upstream: echoes:17007, allow"}}, nil, `l4.routes[1].upstream: service "echoes"`},
		{"l4 address not an input", []edit{{sp, "address_input: ECHO_ADDRESS", "address_input: NOPE"}}, nil, "l4.routes[0].address_input: NOPE is not an input"},
		{"l4 address secret", []edit{{sp, `"Host name or address clients dial the echo service at", default: localhost,`, `"Host name or address clients dial the echo service at", secret: true,`}}, nil, "ECHO_ADDRESS is secret"},
		{"l4 address default", []edit{{sp, "echo service at\", default: localhost,", "echo service at\", default: \"localhost:17007\","}}, nil, `ECHO_ADDRESS's default: "localhost:17007" is not a host name`},
		{"l4 var of no route", []edit{{cp, "${STOWAGE_L4_ECHO}", "${STOWAGE_L4_ECHO_UDP}"}}, nil, "STOWAGE_L4_ECHO_UDP names no L4 route with an address_input"},
		{"l4 allow not an input", []edit{{sp, "allow_input: ECHO_SOURCES", "allow_input: NOPE"}}, nil, "l4.routes[1].allow_input: NOPE is not an input"},
		{"l4 allow asked on docker", []edit{{sp, `default: "0.0.0.0/0 ::/0", shapes: [kairos] }`, `default: "0.0.0.0/0 ::/0", shapes: [docker, kairos] }`}}, nil, "ECHO_SOURCES must be asked on the kairos shape alone"},
		{"l4 allow on every shape", []edit{{sp, `default: "0.0.0.0/0 ::/0", shapes: [kairos] }`, `default: "0.0.0.0/0 ::/0" }`}}, nil, "ECHO_SOURCES must be asked on the kairos shape alone"},
		{"l4 allow secret", []edit{{sp, `default: "0.0.0.0/0 ::/0", shapes: [kairos] }`, `default: "0.0.0.0/0 ::/0", shapes: [kairos], secret: true }`}}, nil, "ECHO_SOURCES is secret: the firewall's rules show it"},
		{"l4 allow advanced", []edit{{sp, `default: "0.0.0.0/0 ::/0", shapes: [kairos] }`, `default: "0.0.0.0/0 ::/0", shapes: [kairos], advanced: true }`}}, nil, "ECHO_SOURCES is advanced, never asked"},
		{"l4 allow by mode", []edit{{sp, `default: "0.0.0.0/0 ::/0", shapes: [kairos] }`, `default: "0.0.0.0/0 ::/0", shapes: [kairos], when: tls }`}}, nil, "ECHO_SOURCES is asked only when tls: the firewall reads it in every mode"},
		{"host network on the VM", []edit{{cp, "    command: [atlas-server, echo, \"17007\"]\n", "    command: [atlas-server, echo, \"17007\"]\n    network_mode: host\n"}}, nil, "services.echo.network_mode: host: the VM's firewall opens only what Stowage publishes"},
		{"host network inherited", []edit{{cp, "    command: [atlas-server, echo, \"17007\"]\n", "    command: [atlas-server, echo, \"17007\"]\n    network_mode: host\n"}, {cp, "  relay:\n", "  relay:\n    extends: echo\n"}}, nil, "services.relay.network_mode: host"},
		{"backup volume not declared", []edit{{sp, "volumes: [spool] ", "volumes: [spools] "}}, nil, `backup.volumes[0]: "spools" is not a volume deploy/compose.yaml declares`},
		{"backup volume external", []edit{{cp, "  spool: {}\n", "  spool: { external: true }\n"}}, nil, `backup.volumes[0]: "spool" is an external volume`},
		{"backup volume interpolated", []edit{{cp, "  spool: {}\n", "  spool: { name: \"${SPOOL}\" }\n"}}, nil, `backup.volumes[0]: "spool" is named "${SPOOL}", which Compose interpolates`},
		{"backup volume of the database", []edit{{sp, "volumes: [spool] ", "volumes: [spool, postgres] "}}, nil, `backup.volumes[1]: "postgres" is mounted by the database's service`},
		{"backup volume key", []edit{{sp, "volumes: [spool] ", "volumes: [\"../spool\"] "}}, nil, `backup: pg-age-rclone: volumes[0] "../spool" is not a compose volume's key`},
		{"backup volume twice", []edit{{sp, "volumes: [spool] ", "volumes: [spool, spool] "}}, nil, `backup: pg-age-rclone: volumes[1] "spool" is listed twice`},
		{"published port on the VM", []edit{{cp, "    command: [atlas-server, echo, \"17007\"]\n", "    command: [atlas-server, echo, \"17007\"]\n    ports: [\"17008:17007\"]\n"}}, nil, "services.echo.ports: the VM's firewall opens only what Stowage publishes"},
		{"profile on kubernetes only", []edit{{sp, "profile: relay, shapes: [docker, kairos]", "profile: relay, shapes: [kubernetes]"}}, nil, "this input is asked on neither"},
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

// Rules of a project without a database or a front: what only a database or a front needs is
// refused where it is declared. examples/stateless being clean is the positive control.
func TestEachRuleFiresOnStateless(t *testing.T) {
	for _, c := range []struct{ name, old, new, want string }{
		{"backup", "\nshapes:\n", "\nbackup:\n  provider: pg-age-rclone\n  profiles: { full: all }\n\nshapes:\n", "backup: a backup dumps the database, and the project declares none"},
		{"data reset", "    state: { uid: 1000 }", "    state: { uid: 1000 }\n  reset: [state]", "data.reset: reset-db resets a database, and the project declares none"},
		{"postgres modes", "kubernetes: { chart: deploy/chart }", "kubernetes: { chart: deploy/chart, postgres_modes: [external] }", "shapes.kubernetes.postgres_modes: the database's modes, and the project declares no database"},
		// Without a front there is no TLS mode to ask an input in.
		{"when without a front", `default: "hello" }`, `default: "hello", when: tls }`, `inputs[0].when: "tls": the project has no front, so no TLS mode: the input would never be asked`},
		// A front section with anything in it is a front, linted as one.
		{"partial front", "\nhealth:\n", "\nfront: { http_port: 8080 }\n\nhealth:\n", "front.tls_modes: at least one TLS mode is required"},
		{"endpoint without a front", "\nshapes:\n", "\nendpoint: lookup\n\nshapes:\n", `endpoint: "lookup" is not a route of front.routes`},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := testutil.CopyExample(t, "stateless")
			testutil.Edit(t, root, specFile, c.old, c.new)
			errs := strings.Join(errorsOf(run(t, root)), "\n")
			if !strings.Contains(errs, c.want) {
				t.Fatalf("no %q in:\n%s", c.want, errs)
			}
		})
	}
	// Without a front no TLS mode is recorded for the stack to read.
	root := testutil.CopyExample(t, "stateless")
	testutil.Edit(t, root, "deploy/compose.yaml", "      - state:/state\n", "      - state:/state\n    environment: { MODE: \"${STOWAGE_TLS_MODE}\" }\n")
	if errs := strings.Join(errorsOf(run(t, root)), "\n"); !strings.Contains(errs, "STOWAGE_TLS_MODE: the project has no front, so no TLS mode is recorded") {
		t.Errorf("STOWAGE_TLS_MODE without a front: %s", errs)
	}
}

// A uid's range is checked on both sides: 0 and the largest user Linux gives are accepted.
func TestVolumeUIDBounds(t *testing.T) {
	t.Parallel()
	for _, uid := range []string{"0", "4294967294"} {
		root := testutil.CopyExample(t, "stateless")
		testutil.Edit(t, root, specFile, "state: { uid: 1000 }", "state: { uid: "+uid+" }")
		if errs := errorsOf(run(t, root)); len(errs) > 0 {
			t.Errorf("uid %s: %v", uid, errs)
		}
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
	testutil.Write(t, root, "stowage.lock", "stowage: 0.2.0\n")
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

// A port bound to a loopback address stays on this host; any other is published past it.
func TestPublishesBeyondLoopback(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		ports any
		want  bool
	}{
		{nil, false}, {[]any{}, false},
		{[]any{"127.0.0.1:6060:6060"}, false}, {[]any{"[::1]:6060:6060/udp"}, false}, {[]any{"::1:6060:6060"}, false},
		{[]any{"${STOWAGE_LOOPBACK_ADDRESS}:5432:5432"}, false}, {[]any{map[string]any{"target": 80, "host_ip": "127.0.0.2"}}, false},
		{[]any{"127.0.0.1:${PG_PORT:-5432}:5432"}, false}, {[]any{"${STOWAGE_LOOPBACK_ADDRESS}:${P:-1}:1"}, false},
		{[]any{"[127.0.0.1]:6060"}, true}, {[]any{"[127.0.0.1]:"}, true}, {[]any{"${HOST:-127.0.0.1}:1:1"}, true},
		{[]any{"$STOWAGE_LOOPBACK_ADDRESS:1:1"}, false}, {[]any{"${STOWAGE_LOOPBACK_ADDRESS:-127.0.0.1}:1:1"}, false},
		{[]any{"${STOWAGE_LOOPBACK_ADDRESS:+0.0.0.0}:1:1"}, true}, {[]any{"${STOWAGE_LOOPBACK_ADDRESSES}:1:1"}, true},
		{[]any{"6060:6060"}, true}, {[]any{"6060"}, true}, {[]any{6060}, true}, {[]any{"0.0.0.0:6060:6060"}, true},
		{[]any{"127.0.0.1:1:1", "192.0.2.1:2:2"}, true}, {[]any{map[string]any{"target": 80}}, true}, {"6060:6060", true},
	} {
		if got := PublishesBeyondLoopback(c.ports); got != c.want {
			t.Errorf("%v: %v", c.ports, got)
		}
	}
}

func FuzzPublishesBeyondLoopback(f *testing.F) {
	for _, s := range []string{"127.0.0.1:1:1", "[::1]:1:1", "[", "::1:1:1", "${STOWAGE_LOOPBACK_ADDRESS}:1:1", "1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		// An entry kept on this host names its host before two port fields.
		if !PublishesBeyondLoopback([]any{p}) && strings.Count(p, ":") < 2 {
			t.Fatalf("%q counted as loopback", p)
		}
	})
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
	testutil.Edit(t, root, specFile, "  provider: pg-age-rclone\n  profiles: { full: all }\n  volumes: [uploads]", "  provider: command\n  image: api\n  cmd: [notes-api, backup]\n")
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

// A route's domain input declared under inputs (to give it a prompt) is read: the front reads it.
func TestDomainInputIsRead(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, specFile, "  - { key: ADMIN_EMAIL,", "  - { key: APP_DOMAIN, prompt: \"The app's domain\", required: true }\n  - { key: ADMIN_EMAIL,")
	for _, f := range run(t, root).Findings {
		t.Errorf("%s", f)
	}
}

// A service that takes its image from the service it extends is clean: compose inherits image:.
func TestExtendsInheritsTheImage(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, composeFile, "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n", "  web:\n    extends: { service: api }\n    image: ${STOWAGE_IMAGE_WEB}\n  worker:\n    extends: api\n")
	for _, f := range run(t, root).Findings {
		t.Errorf("%s", f)
	}
}

// An extends error is its own service's: one extending it is not blamed, and a loop is reported
// once.
func TestExtendsErrorsOnce(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, composeFile, "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n", "  web:\n    image: ${STOWAGE_IMAGE_WEB}\n  a:\n    image: ${STOWAGE_IMAGE_WEB}\n    extends: b\n  b:\n    image: ${STOWAGE_IMAGE_WEB}\n    extends: missing\n  x:\n    image: ${STOWAGE_IMAGE_WEB}\n    extends: y\n  y:\n    image: ${STOWAGE_IMAGE_WEB}\n    extends: x\n")
	var got []string
	for _, e := range errorsOf(run(t, root)) {
		if strings.Contains(e, ".extends") {
			got = append(got, e)
		}
	}
	want := []string{
		`error: deploy/compose.yaml: services.b.extends: extends "missing", which is not a service of this file`,
		"error: deploy/compose.yaml: services.x.extends: extends loops: x → y → x",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got:\n%s", strings.Join(got, "\n"))
	}
}

// The front's ports are TCP: an L4 route over UDP on 443 is clean, beside the front and acme.
func TestL4UDPOnTheFrontsPort(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, specFile, "protocol: udp, port: 17007", "protocol: udp, port: 443")
	if errs := errorsOf(run(t, root)); len(errs) > 0 {
		t.Fatal(strings.Join(errs, "\n"))
	}
}

// A port the compose file publishes past Stowage is warned on the docker shape, whose host
// firewall is the operator's.
func TestPublishedPortOnDockerWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "stateless")
	testutil.Edit(t, root, "deploy/compose.yaml", "    image: ${STOWAGE_IMAGE_SERVER}\n", "    image: ${STOWAGE_IMAGE_SERVER}\n    ports: [\"6060:6060\"]\n")
	res := run(t, root)
	if len(res.Findings) != 1 || res.Findings[0].Severity != Warning || !strings.Contains(res.Findings[0].String(), "ports: published past Stowage") {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// An L4 route's allowlist without a kairos shape is warned: nothing enforces it.
func TestL4AllowlistWithoutKairosWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, specFile, "  kairos:\n    base: ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55\n    kairos_init: quay.io/kairos/kairos-init:v0.16.2@sha256:0777266d25e31b653deaaaa084d4e27d98f7a55e82e42f0648e2ba9c756b8664\n    disk_formats: [raw, qcow2, ova, vhd, gce]\n    cloud_publish: [aws, azure, gcp]\n    health_gate: { deadline_s: 600, deadline_after_reset_s: 1800, poll_s: 30 }\n    host_api: [api]\n    "+vmSizeLine+"   # vm create's least and default size\n", "")
	res := run(t, root)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("errors:\n%s", strings.Join(errs, "\n"))
	}
	if len(res.Findings) != 1 || res.Findings[0].Severity != Warning || !strings.Contains(res.Findings[0].String(), "l4.routes[1].allow_input: only the VM's firewall enforces it, and the project has no kairos shape: the route is open to every source") {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// A service on the host's network with the docker shape alone is warned: it listens past Stowage.
func TestHostNetworkOnDockerWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "stateless")
	testutil.Edit(t, root, "deploy/compose.yaml", "    image: ${STOWAGE_IMAGE_SERVER}\n", "    image: ${STOWAGE_IMAGE_SERVER}\n    network_mode: host\n")
	res := run(t, root)
	if len(res.Findings) != 1 || res.Findings[0].Severity != Warning || !strings.Contains(res.Findings[0].String(), "network_mode: host: the service listens on the host past Stowage") {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// A chart that never reads the L4 routes is warned: they would not be published on Kubernetes.
func TestChartNotReadingTheL4RoutesWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, "deploy/chart/templates/echo.yaml", "{{- range .Values.stowage.l4 }}", "{{- range list }}")
	res := run(t, root)
	if len(res.Findings) != 1 || !strings.Contains(res.Findings[0].String(), "no template reads .Values.stowage.l4") {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// A disk-pressure hook with neither the docker nor the kairos shape is warned: only the VM's host
// daemon and a Docker install's timer act on it.
func TestDiskPressureWithoutKairosOrDockerWarns(t *testing.T) {
	t.Parallel()
	warned := func(res *Result) bool {
		for _, f := range res.Findings {
			if f.Where == "hooks.maintenance.disk_pressure" && strings.Contains(f.Message, "with neither shape, nothing does") {
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
	if warned(run(t, root)) {
		t.Fatal("warned with a docker shape")
	}
	testutil.Edit(t, root, specFile, "  docker: {}\n", "")
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

// A chart that never gives its pods the network settings `k8s install --unsafe-sysctls` sets is
// warned.
func TestChartNotReadingTheSysctlsWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "stateless")
	testutil.Edit(t, root, "deploy/chart/templates/server.yaml", "with .Values.stowage.sysctls", "with .Values.sysctls")
	res := run(t, root)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("errors:\n%s", strings.Join(errs, "\n"))
	}
	if len(res.Findings) != 1 || !strings.Contains(res.Findings[0].Message, "no template reads .Values.stowage.sysctls") {
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

// A service's profiles are its own and those of the service it extends, as compose joins them.
func TestServiceProfilesJoinTheBase(t *testing.T) {
	t.Parallel()
	p, err := ServiceProfiles([]byte("services:\n  base: { image: x, profiles: [a] }\n  child: { extends: base, profiles: [b] }\n  plain: { image: x }\n"))
	if err != nil || !slices.Equal(p["child"], []string{"b", "a"}) || !slices.Equal(p["base"], []string{"a"}) || p["plain"] != nil {
		t.Fatalf("%v %v", p, err)
	}
}

// A service's devices are its host paths, in both syntaxes, joined with those of its base.
func TestServiceDevicesJoinTheBase(t *testing.T) {
	t.Parallel()
	d, err := ServiceDevices([]byte("services:\n  base: { image: x, devices: [\"/dev/ttyUSB0:/dev/ttyS0:rwm\"] }\n" +
		"  child: { extends: base, devices: [{ source: /dev/dri/renderD128, target: /dev/dri/renderD128 }, /dev/net/tun] }\n  plain: { image: x }\n"))
	if err != nil || !slices.Equal(d["child"], []string{"/dev/dri/renderD128", "/dev/net/tun", "/dev/ttyUSB0"}) || !slices.Equal(d["base"], []string{"/dev/ttyUSB0"}) || d["plain"] != nil {
		t.Fatalf("%v %v", d, err)
	}
}

// A volume is mounted by its key in the short syntax and the long one, a bind mount never.
func TestMountsVolume(t *testing.T) {
	t.Parallel()
	vols := []any{"data:/var/lib/data", "./conf:/etc/conf:ro", map[string]any{"type": "volume", "source": "keys", "target": "/keys"},
		map[string]any{"type": "bind", "source": "spool", "target": "/spool"}}
	for key, want := range map[string]bool{"data": true, "keys": true, "spool": false, "conf": false} {
		if mountsVolume(vols, key) != want {
			t.Errorf("%s: want %v", key, want)
		}
	}
}

// An L4 route's address is a host name or an IP address, never with a port, a scheme or a zone.
func TestCheckL4Address(t *testing.T) {
	t.Parallel()
	for v, ok := range map[string]bool{
		"": true, "localhost": true, "data.example.com": true, "192.0.2.7": true, "fd00::7": true,
		"data.example.com:7443": false, "tcp://data.example.com": false, "fe80::1%eth0": false, "Data.example.com": false,
		"[fd00::7]": false, "192.168.1": false, "data.99999999999999999999": false, "10.0.0.300": false, "-data.example.com": false, "data..example.com": false,
	} {
		if err := CheckL4Address(v); (err == nil) != ok {
			t.Errorf("%q: %v", v, err)
		}
	}
}

// Whatever it accepts makes an endpoint net.SplitHostPort reads back as that address.
func FuzzCheckL4Address(f *testing.F) {
	for _, s := range []string{"localhost", "fd00::7", "192.0.2.7", "a.b:1", "[::1]", "x/y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		if CheckL4Address(v) != nil || v == "" {
			return
		}
		host, port, err := net.SplitHostPort(net.JoinHostPort(v, "7443"))
		if err != nil || host != v || port != "7443" || strings.ContainsAny(v, "/ \t\n'\"") {
			t.Fatalf("%q accepted: %q %q %v", v, host, port, err)
		}
	})
}

// memShares is examples/full's stack.memory.shares.
const memShares = "shares: { postgres: { percent: 25, max: 16G }, api: 10 }"

// vmSizeLine is examples/full's shapes.kairos.size.
const vmSizeLine = "size: { min: { cpus: 2, memory: 2G, disk: 20G }, recommended: { cpus: 4, memory: 8G, disk: 40G } }"

// stack.memory's shares at their bounds are clean: 1 % and 90 % each, 90 % together (the errors
// past them are TestEachRuleFiresOnFull's).
func TestMemorySharesBounds(t *testing.T) {
	t.Parallel()
	for _, shares := range []string{"shares: { postgres: 1 }", "shares: { postgres: 90 }", "shares: { postgres: 60, api: 30 }"} {
		root := testutil.CopyExample(t, "full")
		testutil.Edit(t, root, specFile, memShares, shares)
		if errs := errorsOf(run(t, root)); len(errs) > 0 {
			t.Errorf("%s: %v", shares, errs)
		}
	}
}

// A maximum at its bounds is clean: the least limit, the compose file's own limit; a size whose
// minimum equals its recommendation is clean.
func TestMemoryShareMaxAndVMSizeBounds(t *testing.T) {
	t.Parallel()
	for _, e := range [][]string{
		{memShares, "shares: { api: { percent: 10, max: 64M } }"},
		{memShares, "shares: { postgres: { percent: 25, max: 256M } }"},
		{memShares, "shares: { api: { percent: 10, max: 256M } }", "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    deploy: { replicas: \"4\" }\n    image: ${STOWAGE_IMAGE_API}\n"},
		{memShares, "shares: { postgres: { percent: 25, max: 512M } }", "    mem_limit: 256m ", "    scale: 2\n    mem_limit: 256m "},
		{memShares, "shares: { postgres: { percent: 25, max: 1536M } }", "    mem_limit: 256m ", "    mem_reservation: 1.5g\n    mem_limit: 256m "},
		{vmSizeLine, "size: { min: { cpus: 1, memory: 1G, disk: 16G }, recommended: { cpus: 1, memory: 1G, disk: 16G } }"},
		{vmSizeLine, "size: { recommended: { memory: 1025M } }"},
	} {
		root := testutil.CopyExample(t, "full")
		testutil.Edit(t, root, specFile, e[0], e[1])
		if len(e) == 4 {
			testutil.Edit(t, root, "deploy/compose.yaml", e[2], e[3])
		}
		if errs := errorsOf(run(t, root)); len(errs) > 0 {
			t.Errorf("%s: %v", e[1], errs)
		}
	}
}

// Where every host is a VM of at least size.min, a maximum the percent always passes is warned;
// a Docker host can be of any size.
func TestMemoryShareMaxAlwaysReachedWarns(t *testing.T) {
	t.Parallel()
	warned := func(res *Result) bool {
		for _, f := range res.Findings {
			if f.Severity == Warning && f.Where == "stack.memory.shares.postgres.max" {
				return true
			}
		}
		return false
	}
	for share, want := range map[string]bool{
		"{ percent: 25, max: 460M }": true,  // 25 % of the 90 % of 2G its kernel shows is 460.8M: always the maximum
		"{ percent: 25, max: 461M }": false, // just above: under it on the least VM
	} {
		root := testutil.CopyExample(t, "full")
		testutil.Edit(t, root, specFile, memShares, "shares: { postgres: "+share+" }")
		if warned(run(t, root)) {
			t.Errorf("%s: warned with a Docker shape", share)
		}
		testutil.SetYAML(t, root, specFile, []string{"shapes", "docker"}, nil)
		if res := run(t, root); warned(res) != want {
			t.Errorf("%s on the VM alone: warned %v, findings %+v", share, !want, res.Findings)
		}
	}
}

// The nodes are the cluster's: tune on Kubernetes alone is warned.
func TestTuneOnKubernetesAloneWarns(t *testing.T) {
	t.Parallel()
	warned := func(res *Result) bool {
		for _, f := range res.Findings {
			if f.Severity == Warning && f.Where == "tune.profile" {
				return true
			}
		}
		return false
	}
	root := testutil.CopyExample(t, "full")
	testutil.SetYAML(t, root, specFile, []string{"shapes", "docker"}, nil)
	if warned(run(t, root)) {
		t.Fatal("warned with a kairos shape")
	}
	testutil.SetYAML(t, root, specFile, []string{"shapes", "kairos"}, nil)
	if res := run(t, root); !warned(res) {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// The chart sizes its pods: stack.memory on Kubernetes alone is warned.
func TestMemorySharesOnKubernetesAloneWarn(t *testing.T) {
	t.Parallel()
	warned := func(res *Result) bool {
		for _, f := range res.Findings {
			if f.Severity == Warning && f.Where == "stack.memory" {
				return true
			}
		}
		return false
	}
	for _, shape := range []string{"kairos", "docker"} {
		root := testutil.CopyExample(t, "full")
		testutil.SetYAML(t, root, specFile, []string{"shapes", shape}, nil)
		if warned(run(t, root)) {
			t.Fatalf("warned without a %s shape", shape)
		}
	}
	root := testutil.CopyExample(t, "full")
	testutil.SetYAML(t, root, specFile, []string{"shapes", "kairos"}, nil)
	testutil.SetYAML(t, root, specFile, []string{"shapes", "docker"}, nil)
	if res := run(t, root); !warned(res) {
		t.Fatalf("findings: %+v", res.Findings)
	}
}

// A compose file that reads the one other accepted value of a rotated secret reads enough.
func TestRotationReadThroughTheAcceptedValue(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "      INTERNAL_SERVICE_SECRET_NEXT: ${INTERNAL_SERVICE_SECRET_NEXT}\n      INTERNAL_SERVICE_SECRET_PREVIOUS: ${INTERNAL_SERVICE_SECRET_PREVIOUS}\n",
		"      INTERNAL_SERVICE_SECRET_ACCEPTED: ${INTERNAL_SERVICE_SECRET_ACCEPTED}\n")
	if errs := errorsOf(run(t, root)); len(errs) > 0 {
		t.Fatal(errs)
	}
}
