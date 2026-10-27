package lint

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/spec"
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
			// Warnings too: a key the examples use that lint stops counting shows as one.
			for _, f := range res.Findings {
				t.Errorf("%s example: %s", name, f)
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
		{"duplicate domain", []edit{{specFile, "domain_input: API_DOMAIN", "domain_input: APP_DOMAIN"}}, nil, "front.routes[1].domain_input: app serves every path of APP_DOMAIN already: routes of one domain differ by their path"},
		{"duplicate path", []edit{{specFile, "domain_input: API_DOMAIN,", "domain_input: APP_DOMAIN, path: /api,"}, {specFile, "domain_input: APP_DOMAIN, upstream: web:8080", "domain_input: APP_DOMAIN, path: /api, upstream: web:8080"}}, nil, "front.routes[1].path: app serves /api of APP_DOMAIN already"},
		{"paths of one case", []edit{{specFile, "domain_input: API_DOMAIN,", "domain_input: APP_DOMAIN, path: /API,"}, {specFile, "domain_input: APP_DOMAIN, upstream: web:8080", "domain_input: APP_DOMAIN, path: /api, upstream: web:8080"}}, nil, "front.routes[1].path: app serves /API of APP_DOMAIN already"},
		{"https on 80", []edit{{specFile, "loopback_ports:", "http_port: 8080\n  https_port: 80\n  loopback_ports:"}}, nil, "front.https_port: 80 is the front's plain HTTP port in its container"},
		{"path not absolute", []edit{{specFile, "domain_input: API_DOMAIN,", "domain_input: APP_DOMAIN, path: api,"}}, nil, `front.routes[1].path: "api" is not a path prefix`},
		{"path trailing slash", []edit{{specFile, "domain_input: API_DOMAIN,", "domain_input: APP_DOMAIN, path: /api/,"}}, nil, `"/api/" is not a path prefix`},
		{"path root", []edit{{specFile, "domain_input: API_DOMAIN,", "domain_input: APP_DOMAIN, path: /,"}}, nil, `"/" is not a path prefix`},
		{"path wildcard", []edit{{specFile, "domain_input: API_DOMAIN,", "domain_input: APP_DOMAIN, path: /api*,"}}, nil, `"/api*" is not a path prefix`},
		{"path dot segment", []edit{{specFile, "domain_input: API_DOMAIN,", "domain_input: APP_DOMAIN, path: /api/..,"}}, nil, `"/api/.." is not a path prefix`},
		{"client_auth on a shared domain", []edit{{specFile, "domain_input: API_DOMAIN,", "domain_input: APP_DOMAIN, path: /api, client_auth: true,"}}, nil, "front.routes[1].client_auth: APP_DOMAIN is shared with another route"},
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
		{"add_hosts name", []edit{{sp, "packages.atlas.test: 192.0.2.10", "Packages.atlas.test: 192.0.2.10"}}, nil, `images.api.add_hosts: "Packages.atlas.test" is not a host name`},
		{"add_hosts address", []edit{{sp, "packages.atlas.test: 192.0.2.10", "packages.atlas.test: 192.0.2"}}, nil, `images.api.add_hosts.packages.atlas.test: "192.0.2" is not an IP address without a zone`},
		{"add_hosts zone", []edit{{sp, "packages.atlas.test: 192.0.2.10", "packages.atlas.test: fe80::1%eth0"}}, nil, `"fe80::1%eth0" is not an IP address without a zone`},
		{"build network", []edit{{sp, "    network: default\n", "    network: bridge\n"}}, nil, `images.api.network: "bridge" is not a build network: default, host, none`},
		{"build keys with ref", []edit{{sp, "postgres:  { ref: ", "postgres:  { network: none, ref: "}}, nil, "images.postgres: ref and build fields are exclusive"},
		{"memory share of 0", []edit{{sp, memShares, "services:\n  postgres: { memory: 0 }\n"}}, nil, "services.postgres.memory: 0: a percent of the host's RAM, from 1 to 90"},
		{"memory share over 90", []edit{{sp, memShares, "services:\n  postgres: { memory: 91 }\n"}}, nil, "services.postgres.memory: 91: a percent of the host's RAM, from 1 to 90"},
		{"memory shares over 90", []edit{{sp, memShares, "services:\n  postgres: { memory: 60 }\n  api: { memory: 31 }\n"}}, nil, "services: the memory shares total 91 %: at most 90"},
		{"memory share of no service", []edit{{sp, memShares, "services:\n  postgres: { memory: 25 }\n  search: { memory: 10 }\n"}}, nil, `services.search: service "search" is not in deploy/compose.yaml`},
		{"memory share max under the least", []edit{{sp, memShares, "services:\n  api: { memory: { percent: 25, max: 67108863 } }\n"}}, nil, "services.api.memory.max: 67108863: 67108863 for api, under 64M, the least limit"},
		{"memory share max under the least per container", []edit{{sp, memShares, "services:\n  api: { memory: { percent: 10, max: 255M } }\n"}, {"deploy/compose.yaml", "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    deploy: { replicas: \"4\" }\n    image: ${STOWAGE_IMAGE_API}\n"}}, nil, "services.api.memory.max: 255M: 65280K for each of api's 4 containers, under 64M"},
		{"memory share max under a GiB reservation", []edit{{sp, memShares, "services:\n  postgres: { memory: { percent: 25, max: 1535M } }\n"}, {"deploy/compose.yaml", "    mem_limit: 256m ", "    mem_reservation: 1.5GiB\n    mem_limit: 256m "}}, nil, "services.postgres.memory.max: 1535M: 1535M for postgres, under the 1536M limit"},
		{"memory share max under the compose limit", []edit{{sp, memShares, "services:\n  postgres: { memory: { percent: 25, max: 255M } }\n"}}, nil, "services.postgres.memory.max: 255M: 255M for postgres, under the 256M limit or reservation deploy/compose.yaml gives it itself"},
		{"memory share max under the replicas' limits", []edit{{sp, memShares, "services:\n  postgres: { memory: { percent: 25, max: 511M } }\n"}, {"deploy/compose.yaml", "    mem_limit: 256m ", "    scale: 2\n    mem_limit: 256m "}}, nil, "services.postgres.memory.max: 511M: 261632K for each of postgres's 2 containers, under the 256M limit or reservation"},
		{"memory share max under the reservation", []edit{{sp, memShares, "services:\n  postgres: { memory: { percent: 25, max: 1G } }\n"}, {"deploy/compose.yaml", "    mem_limit: 256m ", "    mem_reservation: 1.5g\n    mem_limit: 256m "}}, nil, "services.postgres.memory.max: 1G: 1G for postgres, under the 1536M limit"},
		{"memory share max under the base's limit", []edit{{sp, memShares, "services:\n  api: { memory: { percent: 25, max: 1G } }\n"}, {"deploy/compose.yaml", "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    extends: postgres\n    image: ${STOWAGE_IMAGE_API}\n"}, {"deploy/compose.yaml", "    mem_limit: 256m ", "    mem_limit: 2g "}}, nil, "services.api.memory.max: 1G: 1G for api, under the 2G limit"},
		{"memory share max without percent", []edit{{sp, memShares, "services:\n  postgres: { memory: { max: 16G } }\n"}}, nil, "services.postgres.memory: 0: a percent of the host's RAM"},
		{"vm size empty", []edit{{sp, vmSizeLine, "size: {}"}}, nil, "shapes.kairos.size: states no size"},
		{"vm size states nothing", []edit{{sp, vmSizeLine, "size: { min: {} }"}}, nil, "shapes.kairos.size.min: states nothing"},
		{"vm size negative cpus", []edit{{sp, vmSizeLine, "size: { min: { cpus: -1 } }"}}, nil, "cpus: -1: at least 1"},
		{"vm size small memory", []edit{{sp, vmSizeLine, "size: { min: { memory: 1023M } }"}}, nil, "shapes.kairos.size.min.memory: 1023M: whole MiB, at least 1G"},
		{"vm size partial MiB", []edit{{sp, vmSizeLine, "size: { min: { memory: 2097153K } }"}}, nil, "shapes.kairos.size.min.memory: 2097153K: whole MiB"},
		{"vm size small disk", []edit{{sp, vmSizeLine, "size: { recommended: { disk: 3G } }"}}, nil, "shapes.kairos.size.recommended.disk: 3G: whole GiB, at least 4G"},
		{"vm size partial GiB", []edit{{sp, vmSizeLine, "size: { recommended: { disk: 16385M } }"}}, nil, "shapes.kairos.size.recommended.disk: 16385M: whole GiB"},
		{"vm size min over recommended cpus", []edit{{sp, vmSizeLine, "size: { min: { cpus: 4 }, recommended: { cpus: 3 } }"}}, nil, "shapes.kairos.size.recommended.cpus: 3: under the minimum 4"},
		{"vm size min over recommended memory", []edit{{sp, vmSizeLine, "size: { min: { memory: 8G }, recommended: { memory: 8191M } }"}}, nil, "shapes.kairos.size.recommended.memory: 8191M: under the minimum 8G"},
		{"vm size min over recommended disk", []edit{{sp, vmSizeLine, "size: { min: { disk: 41G }, recommended: { disk: 40G } }"}}, nil, "shapes.kairos.size.recommended.disk: 40G: under the minimum 41G"},
		{"vm size min left empty", []edit{{sp, vmSizeLine, "size: { min: , recommended: { cpus: 2 } }"}}, nil, "shapes.kairos.size.min: states nothing"},
		{"vm size left empty", []edit{{sp, vmSizeLine, "size:"}}, nil, "shapes.kairos.size: states no size"},
		{"memory shares empty", []edit{{sp, servicesBlock, "services: {}\n"}}, nil, "services: names no service"},
		{"services left empty", []edit{{sp, servicesBlock, "services:\n"}}, nil, "services: names no service"},
		{"service asking nothing", []edit{{sp, memShares, "services:\n  api: {}\n"}}, nil, "services.api: says nothing"},
		{"tune profile unknown", []edit{{sp, "  profile: network   #", "  profile: shared   #"}}, nil, `tune.profile: "shared": default or network`},
		{"tune profile missing", []edit{{sp, "  profile: network   #", "  other: network   #"}}, nil, "field other not found"},
		{"tune profile empty", []edit{{sp, "  profile: network   #", "  profile: \"\"   #"}}, nil, "tune.profile: missing"},
		{"tune left empty", []edit{{sp, "tune:\n  profile: network   # a heavy network load: the hosts are tuned for it (default: Docker's path alone)\n", "tune:\n"}}, nil, "tune.profile: missing"},
		{"login user", []edit{{sp, "basic_auth: { user: ops, secret: OPS_PASSWORD }", "basic_auth: { user: \"o:ps\", secret: OPS_PASSWORD }"}}, nil, `front.routes[3].basic_auth.user: "o:ps" must be 1 to 64 letters`},
		{"login user missing", []edit{{sp, "basic_auth: { user: ops, secret: OPS_PASSWORD }", "basic_auth: { secret: OPS_PASSWORD }"}}, nil, `front.routes[3].basic_auth.user: "" must be`},
		{"login secret missing", []edit{{sp, "basic_auth: { user: ops, secret: OPS_PASSWORD }", "basic_auth: { user: ops }"}}, nil, "front.routes[3].basic_auth.secret: is required"},
		{"login secret unknown", []edit{{sp, "basic_auth: { user: ops, secret: OPS_PASSWORD }", "basic_auth: { user: ops, secret: NOPE }"}}, nil, "front.routes[3].basic_auth.secret: NOPE is neither a secret nor an input"},
		{"login secret not secret", []edit{{sp, "basic_auth: { user: ops, secret: OPS_PASSWORD }", "basic_auth: { user: ops, secret: ECHO_ADDRESS }"}}, nil, "front.routes[3].basic_auth.secret: ECHO_ADDRESS is not secret"},
		{"login secret rotated", []edit{{sp, "basic_auth: { user: ops, secret: OPS_PASSWORD }", "basic_auth: { user: ops, secret: INTERNAL_SERVICE_SECRET }"}}, nil, "front.routes[3].basic_auth.secret: INTERNAL_SERVICE_SECRET rotates"},
		{"login beside forward auth", []edit{{sp, "copy_headers: [X-Auth-Request-User] }\n", "copy_headers: [X-Auth-Request-User] }\n      basic_auth: { user: ops, secret: OPS_PASSWORD }\n"}}, nil, "front.routes[1].basic_auth: and forward_auth both ask who the client is"},
		{"login field unknown", []edit{{sp, "basic_auth: { user: ops, secret: OPS_PASSWORD }", "basic_auth: { user: ops, secret: OPS_PASSWORD, realm: x }"}}, nil, "field realm not found"},
		{"l4 port mapped on the vm", []edit{{sp, "port: 17007, upstream: echo:17007, address_input", "port: 17008, upstream: echo:17007, address_input"}}, nil, "l4.routes[0].upstream: echo:17007: on the VM the service runs on the host's network, where nothing maps one port to another: it listens on the route's port, 17008"},
		{"l4 service on a network", []edit{{cp, "  echo:\n    image: ${STOWAGE_IMAGE_INGEST}\n", "  echo:\n    networks: [default]\n    image: ${STOWAGE_IMAGE_INGEST}\n"}}, nil, "deploy/compose.yaml: services.echo.networks: the VM runs echo on the host's network (the L4 route echo), which holds no other"},
		{"l4 service scaled", []edit{{cp, "  echo:\n    image: ${STOWAGE_IMAGE_INGEST}\n", "  echo:\n    scale: 2\n    image: ${STOWAGE_IMAGE_INGEST}\n"}}, nil, "deploy/compose.yaml: services.echo: 2 containers: the VM runs echo on the host's network (the L4 route echo), where one container holds the port"},
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
		{"route without loopback port", []edit{{sp, "loopback_ports: { console: 3000, api: 9090, ingest: 8080, ops: 9091, status: 9092 }", "loopback_ports: { console: 3000, api: 9090, ops: 9091 }"}}, nil, "route ingest has no port"},
		{"loopback ports without none", []edit{{sp, "tls_modes: [acme, dns-cloudflare, byo, internal, none]", "tls_modes: [acme, dns-cloudflare, byo, internal]"}}, nil, "front.loopback_ports: tls_modes does not offer none"},
		{"loopback env without none", []edit{{sp, "tls_modes: [acme, dns-cloudflare, byo, internal, none]", "tls_modes: [acme, dns-cloudflare, byo, internal]"}}, nil, "front.loopback_env: tls_modes does not offer none"},
		{"duplicate loopback port", []edit{{sp, "status: 9092 }", "status: 9090 }"}}, nil, "is already api's port"},
		{"reserved service name", nil, map[string]string{cp: "services:\n  stowage-front:\n    image: ${STOWAGE_IMAGE_API}\n"}, "names starting stowage- are Stowage's own"},
		{"reserved volume name", nil, map[string]string{cp: "services:\n  api:\n    image: ${STOWAGE_IMAGE_API}\nvolumes:\n  stowage-front-data: {}\n"}, "volumes.stowage-front-data: names starting stowage- are Stowage's own"},
		{"compose variable claimed", []edit{{sp, "  COOKIE_SECURE: \"true\"\n", "  COOKIE_SECURE: \"true\"\n  COMPOSE_PROFILES: \"x\"\n"}}, nil, "COMPOSE_PROFILES: the COMPOSE_ prefix is reserved"},
		{"process variable claimed", []edit{{sp, "  COOKIE_SECURE: \"true\"\n", "  COOKIE_SECURE: \"true\"\n  HOME: \"/\"\n"}}, nil, "HOME: the Docker client reads it"},
		{"unrecordable loopback value", []edit{{sp, `loopback_env: { COOKIE_SECURE: "false"`, `loopback_env: { COOKIE_SECURE: "it's false"`}}, nil, "front.loopback_env.COOKIE_SECURE: a value cannot hold a single quote"},
		{"routes sharing a variable", []edit{{sp, "- name: api", "- name: in_gest"}, {sp, "- name: ingest", "- name: in-gest"}, {sp, "loopback_ports: { console: 3000, api: 9090, ingest: 8080, ops: 9091, status: 9092 }", "loopback_ports: { console: 3000, in_gest: 9090, in-gest: 8080, ops: 9091 }"}}, nil, `"in_gest" and "in-gest" both give STOWAGE_URL_IN_GEST`},
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
		{"loopback names no route", []edit{{sp, "status: 9092 }", "status: 9092, admin: 8081 }"}}, nil, "loopback_ports.admin"},
		{"loopback bad port", []edit{{sp, "status: 9092 }", "status: 80800 }"}}, nil, "80800 is not a TCP port"},
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
		{"profile of no service", []edit{{cp, "    profiles: [relay]\n", ""}}, nil, "inputs[13].profile: no service of deploy/compose.yaml has the profile relay"},
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
		{"client auth without a TLS mode", []edit{{sp, "tls_modes: [acme, dns-cloudflare, byo, internal, none]", "tls_modes: [none]"}}, nil, "front.routes[3].client_auth: no TLS mode is offered"},
		{"l4 bind not an input", []edit{{sp, "bind_input: ECHO_BIND", "bind_input: NOPE"}}, nil, "l4.routes[0].bind_input: NOPE is not an input"},
		{"l4 bind asked", []edit{{sp, `(every address when empty)", advanced: true,`, `(every address when empty)",`}}, nil, "l4.routes[0].bind_input: ECHO_BIND must be advanced"},
		{"l4 bind secret", []edit{{sp, `(every address when empty)", advanced: true,`, `(every address when empty)", advanced: true, secret: true,`}}, nil, "ECHO_BIND is secret: the ports the stack publishes show it"},
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
		{"endpoint no route", []edit{{sp, "endpoint: api\n", "endpoint: admin\n"}}, nil, `endpoint: "admin" is not a route of front.routes (console, api, ingest, ops, status)`},
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
		{"kairos base", []edit{{sp, "    base: ubuntu:26.04@sha256:c098726dd9b9733f3e885f8de0ef375206a311b0a317eadc40d347b1115fb944\n", ""}}, nil, "shapes.kairos.base: is required"},
		{"kairos init", []edit{{sp, "    kairos_init: quay.io/kairos/kairos-init:v4.4.0-rc2@sha256:ac885c9f3002a3f0e7000947e3d1e2a5c95f5e2d0212d5f9bbd663347f1ebe0c\n", ""}}, nil, "shapes.kairos.kairos_init: is required"},
		{"kairos base unpinned", []edit{{sp, "base: ubuntu:26.04@sha256:c098726dd9b9733f3e885f8de0ef375206a311b0a317eadc40d347b1115fb944", "base: ubuntu:26.04"}}, nil, `shapes.kairos.base: "ubuntu:26.04" must be pinned by digest`},
		{"kairos init unpinned", []edit{{sp, "kairos-init:v4.4.0-rc2@sha256:ac885c9f3002a3f0e7000947e3d1e2a5c95f5e2d0212d5f9bbd663347f1ebe0c", "kairos-init:v4.4.0-rc2"}}, nil, `"quay.io/kairos/kairos-init:v4.4.0-rc2" must be pinned by digest`},
		{"pull shares the push token", []edit{{sp, "token_env: STOWAGE_PULL_TOKEN", "token_env: STOWAGE_PUSH_TOKEN"}}, nil, "registry.pull: token_env STOWAGE_PUSH_TOKEN is registry.push's"},
		{"pull is the push docker config", []edit{{sp, "pull: { provider: token, user: atlas-pull, token_env: STOWAGE_PULL_TOKEN }", "pull: { provider: docker-config, config: ci }"}, {sp, "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }", "push: { provider: docker-config, config: ci/ }"}}, nil, "registry.pull: is registry.push's Docker config"},
		{"cloud", []edit{{sp, "cloud_publish: [aws, azure, gcp]", "cloud_publish: [ovh]"}}, nil, "unknown cloud"},
		{"host api service", []edit{{sp, "host_api: [api]", "host_api: [admin]"}}, nil, `shapes.kairos.host_api: service "admin" is not in`},
		{"host api twice", []edit{{sp, "host_api: [api]", "host_api: [api, api]"}}, nil, `service "api" is named twice`},
		{"module name", []edit{{sp, "    pcspkr: {", "    pc-spkr: {"}}, nil, "host.modules.pc-spkr: not a kernel module's name"},
		{"module loaded and blocked", []edit{{sp, "pcspkr: { block: true,", "pcspkr: { block: true, load: true,"}}, nil, "host.modules.pcspkr: is both loaded and blocked"},
		{"module blocked with options", []edit{{sp, "pcspkr: { block: true,", "pcspkr: { block: true, options: { x: \"1\" },"}}, nil, "host.modules.pcspkr.options: a blocked module takes no options"},
		{"module says nothing", []edit{{sp, "tcp_diag: { load: true,", "tcp_diag: {"}}, nil, "host.modules.tcp_diag: says nothing"},
		{"module without a reason", []edit{{sp, "reason: a VM has no speaker", "reason: \" \""}}, nil, "host.modules.pcspkr.reason: is required"},
		{"module reason on two lines", []edit{{sp, "reason: a VM has no speaker", "reason: \"a VM\\nhas no speaker\""}}, nil, "host.modules.pcspkr.reason: one line"},
		{"module option name", []edit{{sp, "redirect_always_follow: \"Y\"", "redirect-always-follow: \"Y\""}}, nil, `host.modules.overlay.options: "redirect-always-follow" is not a module parameter's name`},
		{"module option value", []edit{{sp, "redirect_always_follow: \"Y\"", "redirect_always_follow: \"Y N\""}}, nil, `host.modules.overlay.options.redirect_always_follow: "Y N": letters`},
		{"package of the bundle's own", []edit{{sp, "    tcpdump: {", "    linux-image-generic: {"}}, nil, "host.packages.linux-image-generic: is the OS bundle's own (linux-image)"},
		{"package name", []edit{{sp, "    tcpdump: {", "    -tcpdump: {"}}, nil, "host.packages.-tcpdump: not a package's name"},
		{"package without a reason", []edit{{sp, "reason: \"capture on the VM's interfaces\"", "reason: \"\""}}, nil, "host.packages.tcpdump.reason: is required"},
		{"firmware path", []edit{{sp, "    intel/ice/ddp/ice.pkg: {", "    intel/../ice.pkg: {"}}, nil, "host.firmware.intel/../ice.pkg: not a firmware file's path under /lib/firmware"},
		{"interface name", []edit{{sp, "    edge:   ", "    Edge:   "}, {sp, "interface: edge }", "interface: Edge }"}, {sp, "{ interface: edge, address_input", "{ interface: Edge, address_input"}}, nil, "host.interfaces.Edge: not an interface's name"},
		{"interface named as the kernel names", []edit{{sp, "    vpn0: { kind: tun", "    eth1: { kind: tun"}}, nil, "host.interfaces.eth1: a name the kernel, udev or Docker gives"},
		{"interface kind", []edit{{sp, "vpn0: { kind: tun,", "vpn0: { kind: wifi,"}}, nil, `host.interfaces.vpn0.kind: "wifi": nic (the default) or tun`},
		{"no primary interface", []edit{{sp, "    uplink: { primary: true, ", "    uplink: { "}}, nil, "host.interfaces: 0 NICs are primary"},
		{"a primary tun", []edit{{sp, "vpn0: { kind: tun,", "vpn0: { kind: tun, primary: true,"}}, nil, "host.interfaces.vpn0.primary: a tun device a service opens is not the default route's"},
		{"interface MTU", []edit{{sp, "      mtu: 1500\n", "      mtu: 1279\n"}}, nil, "host.interfaces.edge.mtu: 1279: 1280 to 9216"},
		{"routing setting", []edit{{sp, `FirewallMark: "0x1"`, `ExecStart: "0x1"`}}, nil, `host.interfaces.edge.routing_policy_rules[0]: "ExecStart" is not one of networkd's settings here`},
		{"routing value", []edit{{sp, `Table: "100" }]
      routes`, `Table: "100; x" }]
      routes`}}, nil, `host.interfaces.edge.routing_policy_rules[0].Table: "100; x": letters, digits`},
		{"route of a policy rule's key", []edit{{sp, "routes: [{ Type: local,", "routes: [{ FirewallMark: local,"}}, nil, `host.interfaces.edge.routes[0]: "FirewallMark" is not one of networkd's settings here`},
		{"front on an undeclared interface", []edit{{sp, "  interface: uplink ", "  interface: wan "}}, nil, `front.interface: "wan" is not one of host.interfaces`},
		{"route on an undeclared interface", []edit{{sp, "interface: edge }", "interface: wan }"}}, nil, `l4.routes[1].interface: "wan" is not one of host.interfaces`},
		{"ethtool channels", []edit{{sp, "channels: { combined: max }", "channels: { combined: lots }"}}, nil, `host.interfaces.edge.ethtool.channels.combined: "lots": max, or a count`},
		{"ethtool feature", []edit{{sp, `features: { rx-gro-list: "off" }`, `features: { rx-gro-list: "maybe" }`}}, nil, `host.interfaces.edge.ethtool.features.rx-gro-list: "maybe": on or off`},
		{"ethtool on a tun", []edit{{sp, "vpn0: { kind: tun,", "vpn0: { kind: tun, ethtool: { eee: \"off\" },"}}, nil, "host.interfaces.vpn0.ethtool: a tun device has no NIC's settings"},
		{"service cpus", []edit{{sp, "echo: { cpus: 1, irq: on }", "echo: { cpus: 65, irq: on }"}}, nil, "services.echo.cpus: 65: whole cores, 1 to 64"},
		{"service irq", []edit{{sp, "echo: { cpus: 1, irq: on }", "echo: { cpus: 1, irq: maybe }"}}, nil, `services.echo.irq: "maybe": off`},
		{"service irq without cpus", []edit{{sp, "echo: { cpus: 1, irq: on }", "echo: { irq: on }"}}, nil, "services.echo.irq: is about the service's cores: give it cpus"},
		{"service of no compose service", []edit{{sp, "echo: { cpus: 1, irq: on }", "echoes: { cpus: 1 }"}}, nil, `services.echoes: service "echoes" is not in deploy/compose.yaml`},
		{"service nftables table of another name", []edit{{"deploy/ingest.nft", "table inet ingest {", "table inet other {"}}, nil, `services.ingest.nftables: deploy/ingest.nft: does not start with "table inet ingest {"`},
		{"service nftables beyond its table", []edit{{"deploy/ingest.nft", "\t\tip saddr @probes counter\n", "\t\tip saddr @probes counter\n\t\tinclude \"/etc/x.nft\"\n"}}, nil, "services.ingest.nftables: deploy/ingest.nft: holds an include or names the ruleset"},
		{"service nftables closing its table in a string", []edit{{"deploy/ingest.nft", "\t\tip saddr @probes counter\n\t}\n}\n", "\t\tip saddr @probes counter comment \"{\"\n\t}\n}\ndelete table inet atlas\ntable inet ingest { comment \"}\" }\n"}}, nil, "services.ingest.nftables: deploy/ingest.nft: holds something after its table"},
		{"service nftables after its table", []edit{{"deploy/ingest.nft", "\t\tip saddr @probes counter\n\t}\n}\n", "\t\tip saddr @probes counter\n\t}\n}\ntable ip x {}\n"}}, nil, "services.ingest.nftables: deploy/ingest.nft: holds something after its table"},
		{"service nftables unbalanced", []edit{{"deploy/ingest.nft", "\t\tip saddr @probes counter\n\t}\n}\n", "\t\tip saddr @probes counter\n\t}\n"}}, nil, "services.ingest.nftables: deploy/ingest.nft: its braces do not balance"},
		{"service nftables missing", []edit{{sp, "nftables: deploy/ingest.nft", "nftables: deploy/none.nft"}}, nil, "services.ingest.nftables: open "},
		{"service interface without its input", []edit{{sp, "{ interface: edge, address_input: RELAY_ADDRESS }", "{ interface: edge }"}}, nil, "services.relay: interface and address_input go together"},
		{"service interface not a NIC", []edit{{sp, "{ interface: edge, address_input: RELAY_ADDRESS }", "{ interface: vpn0, address_input: RELAY_ADDRESS }"}}, nil, `services.relay.interface: "vpn0" is not a NIC of host.interfaces`},
		{"service address input unknown", []edit{{sp, "address_input: RELAY_ADDRESS }", "address_input: NOPE }"}}, nil, "services.relay.address_input: NOPE is not an input"},
		{"service address default", []edit{{sp, `prompt: "Address the relay has of its own on the edge network (empty: none)", optional: true,`, `prompt: "Address the relay has of its own on the edge network (empty: none)", default: nowhere,`}}, nil, `services.relay.address_input: RELAY_ADDRESS's default "nowhere" is not an address`},
		{"service on the host's network with an address", []edit{{"deploy/compose.yaml", "    profiles: [relay]\n", "    profiles: [relay]\n    network_mode: host\n"}}, nil, "services.relay.interface: relay runs with network_mode host"},
		{"update order", []edit{{sp, "update: start-first,", "update: rolling,"}}, nil, `services.api.update: "rolling": stop-first (the default) or start-first`},
		{"update socket without start-first", []edit{{sp, "update: start-first, update_signal: SIGUSR2, update_socket:", "update_socket:"}}, nil, "services.api.update_socket: is the directory a start-first overlap hands its sockets over in"},
		{"update socket relative", []edit{{sp, "update_socket: /run/atlas/handoff", "update_socket: run/atlas"}}, nil, `services.api.update_socket: "run/atlas": a directory of the container`},
		{"update socket /", []edit{{sp, "update_socket: /run/atlas/handoff", "update_socket: /"}}, nil, `services.api.update_socket: "/": a directory of the container`},
		{"update signal without start-first", []edit{{sp, "update: start-first, update_signal: SIGUSR2", "update_signal: SIGUSR2"}}, nil, "services.api.update_signal: is what a start-first overlap sends the old container"},
		{"update signal unknown", []edit{{sp, "update_signal: SIGUSR2", "update_signal: SIGKILL"}}, nil, `services.api.update_signal: "SIGKILL": one of SIGHUP, SIGINT, SIGQUIT, SIGTERM, SIGUSR1, SIGUSR2, SIGWINCH`},
		{"start-first with an address", []edit{{sp, "address_input: RELAY_ADDRESS }", "address_input: RELAY_ADDRESS, update: start-first }"}}, nil, "services.relay.update: relay has an address of its own"},
		{"start-first with a container name", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    container_name: atlas-api\n    image: ${STOWAGE_IMAGE_API}\n"}}, nil, "services.api.update: api sets container_name atlas-api"},
		{"start-first with replicas", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    deploy: { replicas: 2 }\n    image: ${STOWAGE_IMAGE_API}\n"}}, nil, "services.api.update: api runs 2 containers"},
		{"start-first publishing a port", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    ports: [\"127.0.0.1:9090:9090\"]\n    image: ${STOWAGE_IMAGE_API}\n"}}, nil, "services.api.update: api publishes ports from its own network"},
		{"start-first without its healthcheck", []edit{{cp, "    healthcheck:\n      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n      interval: 2s\n      retries: 30\n", "    healthcheck: { disable: true }\n"}}, nil, "services.api.update: api disables its healthcheck"},
		{"start-first with replicas from a variable", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    deploy: { replicas: \"${API_REPLICAS}\" }\n    image: ${STOWAGE_IMAGE_API}\n"}}, nil, `services.api.update: api's number of containers is "${API_REPLICAS}", which only a run knows`},
		{"start-first with a NONE healthcheck", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"NONE\"]\n"}}, nil, "services.api.update: api disables its healthcheck"},
		{"start-first with a namespace sidecar", []edit{{cp, "    profiles: [relay]\n", "    profiles: [relay]\n    network_mode: service:api\n"}}, nil, "services.api.update: relay joins api's namespace (network_mode: service:api)"},
		{"start-first with a pid sidecar", []edit{{cp, "    profiles: [relay]\n", "    profiles: [relay]\n    pid: service:api\n"}}, nil, "services.api.update: relay joins api's namespace (pid: service:api)"},
		{"start-first with an ipc sidecar", []edit{{cp, "    profiles: [relay]\n", "    profiles: [relay]\n    ipc: service:api\n"}}, nil, "services.api.update: relay joins api's namespace (ipc: service:api)"},
		{"start-first lending its volumes", []edit{{cp, "    profiles: [relay]\n", "    profiles: [relay]\n    volumes_from: [\"api:ro\"]\n"}}, nil, "services.api.update: relay mounts api's volumes (volumes_from: api:ro)"},
		{"start-first in another's namespace", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    network_mode: service:ui\n    image: ${STOWAGE_IMAGE_API}\n"}}, nil, "services.api.update: api runs in another's network namespace (network_mode service:ui)"},
		{"start-first lending its volumes through extends", []edit{{cp, "    profiles: [relay]\n", "    profiles: [relay]\n    extends: ui\n"}, {cp, "  ui:\n    image: ${STOWAGE_IMAGE_UI}\n", "  ui:\n    volumes_from: [api]\n    image: ${STOWAGE_IMAGE_UI}\n"}}, nil, "services.api.update: relay mounts api's volumes (volumes_from: api)"},
		{"start-first with a disabled base healthcheck", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    extends: ui\n    image: ${STOWAGE_IMAGE_API}\n"}, {cp, "  ui:\n    image: ${STOWAGE_IMAGE_UI}\n", "  ui:\n    healthcheck: { disable: true }\n    image: ${STOWAGE_IMAGE_UI}\n"}}, nil, "services.api.update: api disables its healthcheck"},
		{"start-first with a test of no probe", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"curl\", \"-f\", \"http://localhost:9090\"]\n"}}, nil, "services.api.update: api's healthcheck test starts with curl, not CMD, CMD-SHELL or NONE"},
		{"start-first with a test of no command", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD-SHELL\", \"\"]\n"}}, nil, "services.api.update: api's healthcheck test runs no command"},
		{"start-first with a blank string test", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: \" \"\n"}}, nil, "api's healthcheck test runs no command"},
		{"start-first with an empty string test", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: \"\"\n"}}, nil, "api's healthcheck test runs no command"},
		{"start-first with CMD alone", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD\"]\n"}}, nil, "api's healthcheck test runs no command"},
		{"start-first with a blank CMD-SHELL and args", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD-SHELL\", \"\", \"probe\"]\n"}}, nil, "api's healthcheck test runs no command"},
		{"start-first with a blank CMD and args", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD\", \" \", \"probe\"]\n"}}, nil, "api's healthcheck test runs no command"},
		{"start-first with a NONE string", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: NONE\n"}}, nil, "api's healthcheck test is the string NONE"},
		{"start-first with a command not a string", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD\", 1]\n"}}, nil, "api's healthcheck test has a command (after CMD) that is not a string"},
		{"start-first with disable as a string", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n      disable: \"true\"\n"}}, nil, "services.api.update: api disables its healthcheck"},
		{"start-first with disable: yes", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n      disable: yes\n"}}, nil, "services.api.update: api disables its healthcheck"},
		{"start-first with a fixed address", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    networks: { default: { ipv4_address: 172.30.0.5 } }\n    image: ${STOWAGE_IMAGE_API}\n"}}, nil, "services.api.update: api sets networks.default.ipv4_address"},
		{"start-first with a fixed MAC", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    mac_address: 02:42:ac:11:00:02\n    image: ${STOWAGE_IMAGE_API}\n"}}, nil, "services.api.update: api sets mac_address"},
		{"start-first with a broken test from a variable", []edit{{cp, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"curl\", \"-f\", \"http://localhost:${API_PORT:-9090}\"]\n"}}, nil, "api's healthcheck test starts with curl"},
		{"start-first inheriting a fixed address", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    extends: ui\n    image: ${STOWAGE_IMAGE_API}\n"}, {cp, "  ui:\n    image: ${STOWAGE_IMAGE_UI}\n", "  ui:\n    mac_address: 02:42:ac:11:00:02\n    image: ${STOWAGE_IMAGE_UI}\n"}}, nil, "services.api.update: api sets mac_address"},
		{"start-first inheriting a base network's address", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    extends: ui\n    networks: { default: { aliases: [atlas-api] } }\n    image: ${STOWAGE_IMAGE_API}\n"}, {cp, "  ui:\n    image: ${STOWAGE_IMAGE_UI}\n", "  ui:\n    networks: { default: { ipv4_address: 172.30.0.5 } }\n    image: ${STOWAGE_IMAGE_UI}\n"}}, nil, "services.api.update: api sets networks.default.ipv4_address"},
		{"L4 upstream inheriting networks", []edit{{cp, "  echo:\n", "  echo:\n    extends: ui\n"}, {cp, "  ui:\n    image: ${STOWAGE_IMAGE_UI}\n", "  ui:\n    networks: [default]\n    image: ${STOWAGE_IMAGE_UI}\n"}}, nil, "deploy/compose.yaml: services.ui.networks: echo extends ui, whose networks it takes"},
		{"start-first linked to", []edit{{cp, "    profiles: [relay]\n", "    profiles: [relay]\n    links: [\"api:www\"]\n"}}, nil, "services.api.update: relay links to api (links: api:www)"},
		{"start-first linked to through extends", []edit{{cp, "    profiles: [relay]\n", "    profiles: [relay]\n    extends: ui\n"}, {cp, "  ui:\n    image: ${STOWAGE_IMAGE_UI}\n", "  ui:\n    links: [api]\n    image: ${STOWAGE_IMAGE_UI}\n"}}, nil, "services.api.update: relay links to api (links: api)"},
		{"start-first inheriting a container name", []edit{{cp, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    extends: ui\n    image: ${STOWAGE_IMAGE_API}\n"}, {cp, "  ui:\n    image: ${STOWAGE_IMAGE_UI}\n", "  ui:\n    container_name: atlas-ui\n    image: ${STOWAGE_IMAGE_UI}\n"}}, nil, "services.api.update: api sets container_name atlas-ui"},
		{"firmware without a reason", []edit{{sp, "reason: \"the E810's DDP package: without it ice runs in safe mode, one queue a port\"", "reason: \"\""}}, nil, "host.firmware.intel/ice/ddp/ice.pkg.reason: is required"},
		{"package key unknown", []edit{{sp, "tcpdump: { reason:", "tcpdump: { version: \"4\", reason:"}}, nil, "field version not found"},
		{"kernel arg of Kairos's own", []edit{{sp, "    iommu: { value: pt,", "    rd.shell: { value: \"1\","}}, nil, "host.kernel_args.rd.shell: is Kairos's own (rd)"},
		{"kernel arg name", []edit{{sp, "    iommu: { value: pt,", "    \"io mmu\": { value: pt,"}}, nil, "host.kernel_args.io mmu: not a kernel argument's name"},
		{"kernel arg value", []edit{{sp, "    iommu: { value: pt,", "    iommu: { value: \"pt\\\"\","}}, nil, "host.kernel_args.iommu.value: \"pt\\\"\": letters"},
		{"kernel arg marks a fallback", []edit{{sp, "    iommu: { value: pt,", "    iommu: { value: \"pt,upgrade_failure\","}}, nil, `host.kernel_args.iommu.value: "pt,upgrade_failure" holds upgrade_failure`},
		{"kernel arg spelt with _", []edit{{sp, "    iommu: { value: pt,", "    net.naming_scheme: { value: v238,"}}, nil, "host.kernel_args.net.naming_scheme: is Kairos's own (net.naming-scheme)"},
		{"kernel arg without a reason", []edit{{sp, "reason: \"the NICs' DMA passed through, not translated\"", "reason: \"\""}}, nil, "host.kernel_args.iommu.reason: is required"},
		{"sysctl of a profile", []edit{{sp, "    vm.swappiness: {", "    net.netfilter.nf_conntrack_max: {"}}, nil, "host.sysctls.net.netfilter.nf_conntrack_max: is a tune profile's"},
		{"sysctl key", []edit{{sp, "    vm.swappiness: {", "    vm/swappiness: {"}}, nil, "host.sysctls.vm/swappiness: not a kernel setting's key"},
		{"sysctl value", []edit{{sp, "vm.swappiness: { value: \"10\"", "vm.swappiness: { value: \"10;reboot\""}}, nil, `host.sysctls.vm.swappiness.value: "10;reboot"`},
		{"sysctl without a reason", []edit{{sp, "reason: \"postgres's pages stay in RAM\"", "reason: \"\""}}, nil, "host.sysctls.vm.swappiness.reason: is required"},
		{"power", []edit{{sp, "  power: performance", "  power: turbo"}}, nil, `host.power: "turbo": performance or platform`},
		{"module key unknown", []edit{{sp, "pcspkr: { block: true,", "pcspkr: { blocked: true,"}}, nil, "field blocked not found"},
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
	testutil.Edit(t, root, specFile, "{ console: 3000, api: 9090, ingest: 8080, ops: 9091, status: 9092 }", "{ console: 0, api: 70000, ingest: 99999, ops: 9091, a: 1, b: 2 }")
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

// nextKey is the next top-level or shapes key of a spec: where a shape's block ends.
var nextKey = regexp.MustCompile(`\n(  )?[a-z]`)

// A start-first service without a healthcheck of its compose file is warned (the image's own is
// what the overlap waits for), as is one an L4 route targets with the Docker shape (its port is
// published there, so it is replaced stop-first) and with the kairos shape (both containers
// listen on its port there); the example's api warns of none.
func TestStartFirstWarnings(t *testing.T) {
	t.Parallel()
	warned := func(res *Result, msg string) bool {
		for _, f := range res.Findings {
			if f.Severity == Warning && strings.Contains(f.Message, msg) {
				return true
			}
		}
		return false
	}
	const health, l4 = "api sets no healthcheck", "echo is l4.routes[0]'s upstream: on Docker"
	root := testutil.CopyExample(t, "full")
	if res := run(t, root); warned(res, health) || warned(res, "upstream: on Docker") {
		t.Fatalf("the example warns: %+v", res.Findings)
	}
	testutil.Edit(t, root, composeFile, "    healthcheck:\n      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n      interval: 2s\n      retries: 30\n", "")
	testutil.Edit(t, root, specFile, "echo: { cpus: 1, irq: on }", "echo: { cpus: 1, irq: on, update: start-first }")
	if res := run(t, root); !warned(res, health) || !warned(res, l4) || !warned(res, "echo is l4.routes[0]'s upstream: on the VM it runs on the host's network") {
		t.Fatalf("findings: %+v", res.Findings)
	}
	testutil.Edit(t, root, composeFile, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"8080\"]\n", "")
	testutil.Edit(t, root, specFile, "ingest: { nftables: deploy/ingest.nft }", "ingest: { nftables: deploy/ingest.nft, update: start-first }")
	if res := run(t, root); !warned(res, "ingest's healthcheck sets no test") {
		t.Fatalf("findings: %+v", res.Findings)
	}
	// A healthcheck merges key by key through extends: the base's test with the service's interval.
	testutil.Edit(t, root, composeFile, "  ingest:\n", "  ingest:\n    extends: relay\n")
	if res := run(t, root); warned(res, "ingest's healthcheck sets no test") || warned(res, "ingest sets no healthcheck") {
		t.Fatalf("an inherited test: %+v", res.Findings)
	}
	// The service's own test and disable win over its base's.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    extends: ui\n    image: ${STOWAGE_IMAGE_API}\n")
	testutil.Edit(t, root, composeFile, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n      disable: false\n")
	testutil.Edit(t, root, composeFile, "  ui:\n    image: ${STOWAGE_IMAGE_UI}\n", "  ui:\n    healthcheck: { test: [\"NONE\"], disable: true }\n    image: ${STOWAGE_IMAGE_UI}\n")
	if res := run(t, root); slices.ContainsFunc(res.Findings, func(f Finding) bool { return strings.Contains(f.Message, "api disables its healthcheck") }) {
		t.Fatalf("its own healthcheck: %+v", res.Findings)
	}
	// On the host's network without update_socket, both containers listen on its ports.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, specFile, ", update_socket: /run/atlas/handoff }", " }")
	testutil.Edit(t, root, composeFile, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    network_mode: host\n    image: ${STOWAGE_IMAGE_API}\n")
	if res := run(t, root); !warned(res, "api runs on the host's network, where both containers of an overlap listen on its ports") {
		t.Fatalf("host network: %+v", res.Findings)
	}
	// With update_socket it is the app's handoff: no warning.
	testutil.Edit(t, root, specFile, "update_signal: SIGUSR2 }", "update_signal: SIGUSR2, update_socket: /run/atlas/handoff }")
	if res := run(t, root); warned(res, "api runs on the host's network") {
		t.Fatalf("host network with update_socket: %+v", res.Findings)
	}
	// An L4 upstream: on the VM, warned without update_socket alone; on Docker, warned unless on
	// the host's network.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, specFile, "echo: { cpus: 1, irq: on }", "echo: { cpus: 1, irq: on, update: start-first, update_socket: /run/echo }")
	if res := run(t, root); warned(res, "echo is l4.routes[0]'s upstream: on the VM") || !warned(res, l4) {
		t.Fatalf("an L4 upstream with update_socket: %+v", res.Findings)
	}
	testutil.Edit(t, root, composeFile, "    command: [atlas-server, echo, \"17007\"]\n", "    command: [atlas-server, echo, \"17007\"]\n    network_mode: host\n")
	if res := run(t, root); warned(res, l4) {
		t.Fatalf("an L4 upstream on the host's network: %+v", res.Findings)
	}
	// Each L4 warning is its shape's: without the kairos shape no VM warning, without the docker
	// shape no Docker one.
	for _, c := range []struct{ shape, gone, kept string }{
		{"kairos", "echo is l4.routes[0]'s upstream: on the VM", l4},
		{"docker", l4, "echo is l4.routes[0]'s upstream: on the VM"},
	} {
		root = testutil.CopyExample(t, "full")
		testutil.Edit(t, root, specFile, "echo: { cpus: 1, irq: on }", "echo: { cpus: 1, irq: on, update: start-first }")
		raw, err := os.ReadFile(filepath.Join(root, specFile))
		if err != nil {
			t.Fatal(err)
		}
		y := string(raw)
		start := strings.Index(y, "\n  "+c.shape+":")
		if start < 0 {
			t.Fatalf("the example has no %s shape", c.shape)
		}
		end := len(y)
		if next := nextKey.FindStringIndex(y[start+1:]); next != nil {
			end = start + 1 + next[0]
		}
		testutil.Write(t, root, specFile, y[:start]+y[end:])
		if res := run(t, root); warned(res, c.gone) || !warned(res, c.kept) {
			t.Fatalf("without the %s shape: %+v", c.shape, res.Findings)
		}
	}
	// disable as a string that casts to false leaves the healthcheck on.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n      disable: \"off\"\n")
	if res := run(t, root); slices.ContainsFunc(res.Findings, func(f Finding) bool { return strings.Contains(f.Message, "api disables its healthcheck") }) {
		t.Fatalf("disable: off: %+v", res.Findings)
	}
	// disable that Compose does not read as a boolean is warned; a $$ escape is no variable.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD-SHELL\", \"atlas-server probe $$((9000 + 90))\"]\n")
	if res := run(t, root); warned(res, "api's healthcheck test comes from a variable") {
		t.Fatalf("an escaped $: %+v", res.Findings)
	}
	testutil.Edit(t, root, composeFile, "$$((9000 + 90))\"]\n", "$$((9000 + 90))\"]\n      disable: \"1\"\n")
	if res := run(t, root); !warned(res, "api's healthcheck disable is 1, which only a run knows or Compose does not read as a boolean") {
		t.Fatalf("disable 1: %+v", res.Findings)
	}
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n      disable: ${HC_OFF:-false}\n")
	if res := run(t, root); !warned(res, "api's healthcheck disable is ${HC_OFF:-false}, which only a run knows") {
		t.Fatalf("disable from a variable: %+v", res.Findings)
	}
	// A test whose head comes from a variable is warned, not refused; with a disable from a
	// variable, both are warned.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"${HC_KIND:-CMD}\", \"atlas-server\", \"probe\", \"9090\"]\n      disable: ${HC_OFF:-false}\n")
	if res := run(t, root); !warned(res, "api's healthcheck test comes from a variable") || !warned(res, "api's healthcheck disable is ${HC_OFF:-false}") ||
		slices.ContainsFunc(res.Findings, func(f Finding) bool { return f.Severity == Error && strings.Contains(f.Message, "api's healthcheck") }) {
		t.Fatalf("a head from a variable: %+v", res.Findings)
	}
	// A service joining a container by name may be joining the start-first service's: warned.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "    profiles: [relay]\n", "    profiles: [relay]\n    ipc: container:atlas-api-1\n")
	testutil.Edit(t, root, composeFile, "    ipc: container:atlas-api-1\n", "    ipc: container:atlas-api-1\n    volumes_from: [\"container:atlas-api-1:ro\", \"container:atlas-api-2\"]\n")
	res := run(t, root)
	n := 0
	for _, f := range res.Findings {
		if strings.Contains(f.Message, "joins a container by its name or ID") {
			n++
		}
	}
	if !warned(res, "joins a container by its name or ID (container:atlas-api-1)") || !warned(res, "joins a container by its name or ID (container:atlas-api-1:ro)") || n != 2 {
		t.Fatalf("a container joined by name: %+v", res.Findings)
	}
	// One from a variable is warned too; a start-first service's own network_mode is an error alone.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "    profiles: [relay]\n", "    profiles: [relay]\n    ipc: ${RELAY_IPC:-service:api}\n")
	testutil.Edit(t, root, composeFile, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    network_mode: container:vpn\n    image: ${STOWAGE_IMAGE_API}\n")
	res = run(t, root)
	if !warned(res, "${RELAY_IPC:-service:api}, which only a run resolves") || warned(res, "(container:vpn)") ||
		!slices.ContainsFunc(res.Findings, func(f Finding) bool {
			return f.Severity == Error && strings.Contains(f.Message, "api runs in another's network namespace (network_mode container:vpn)")
		}) {
		t.Fatalf("a join from a variable, the service's own: %+v", res.Findings)
	}
	// A start-first service's own network_mode from a variable is warned.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    network_mode: ${API_NET:-host}\n    ports: [\"127.0.0.1:9090:9090\"]\n    image: ${STOWAGE_IMAGE_API}\n")
	if res := run(t, root); !warned(res, "${API_NET:-host}, which only a run resolves: if it is service:… or container:…, or a network of its own while it publishes ports, the install replaces api stop-first") || warned(res, "api stop-first, and if it is host") ||
		slices.ContainsFunc(res.Findings, func(f Finding) bool {
			return f.Severity == Error && strings.Contains(f.Message, "api publishes ports from its own network")
		}) {
		t.Fatalf("its own network_mode from a variable: %+v", res.Findings)
	}
	// A start-first service named after a keyword is no volumes_from of a container.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "    profiles: [relay]\n", "    profiles: [relay]\n    volumes_from: [\"container:atlas-db-1\"]\n")
	testutil.Edit(t, root, specFile, "echo: { cpus: 1, irq: on }", "echo: { cpus: 1, irq: on, update: start-first }")
	if res := run(t, root); slices.ContainsFunc(res.Findings, func(f Finding) bool { return strings.Contains(f.Message, "mounts echo's volumes") }) {
		t.Fatalf("an unrelated container's volumes: %+v", res.Findings)
	}
	// external_links name a container: warned as one; an unbraced $VAR network_mode is a variable,
	// which spares an L4 upstream the Docker warning.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "    profiles: [relay]\n", "    profiles: [relay]\n    external_links: [\"atlas-api-1:api\"]\n")
	testutil.Edit(t, root, specFile, "echo: { cpus: 1, irq: on }", "echo: { cpus: 1, irq: on, update: start-first }")
	testutil.Edit(t, root, composeFile, "    command: [atlas-server, echo, \"17007\"]\n", "    command: [atlas-server, echo, \"17007\"]\n    network_mode: $ECHO_NET\n")
	res = run(t, root)
	if !warned(res, "links to a container by its name (atlas-api-1:api)") || !warned(res, "$ECHO_NET, which only a run resolves: if it is service:… or container:…, or a network of its own while it publishes ports (its L4 route's on Docker included), the install replaces echo stop-first") || warned(res, l4) {
		t.Fatalf("external_links, an unbraced variable: %+v", res.Findings)
	}
	// Without ports, its own network_mode from a variable names no ports case; service:${X} is the
	// refusal's alone.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    network_mode: ${API_NET:-bridge}\n    image: ${STOWAGE_IMAGE_API}\n")
	if res := run(t, root); !warned(res, "${API_NET:-bridge}, which only a run resolves") || warned(res, "while it publishes ports") {
		t.Fatalf("no ports: %+v", res.Findings)
	}
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    network_mode: service:${PEER:-ui}\n    image: ${STOWAGE_IMAGE_API}\n")
	if res := run(t, root); warned(res, "which only a run resolves") {
		t.Fatalf("service:${X}: %+v", res.Findings)
	}
	// links from a variable is warned; a non-string entry is none.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "    profiles: [relay]\n", "    profiles: [relay]\n    links: [\"${PEER:-api}\"]\n    external_links: [123]\n")
	res = run(t, root)
	if !warned(res, "${PEER:-api}, which only a run resolves: if it names a start-first service, the install") || warned(res, "links to a container by its name") {
		t.Fatalf("links from a variable: %+v", res.Findings)
	}
	// A test from a variable is warned: lint cannot tell it runs a probe.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: [\"CMD-SHELL\", \"${API_PROBE:-atlas-server probe 9090}\"]\n")
	if res := run(t, root); !warned(res, "api's healthcheck test comes from a variable") {
		t.Fatalf("a test from a variable: %+v", res.Findings)
	}
	// An empty test is the image's.
	root = testutil.CopyExample(t, "full")
	testutil.Edit(t, root, composeFile, "      test: [\"CMD\", \"atlas-server\", \"probe\", \"9090\"]\n", "      test: []\n")
	if res := run(t, root); !warned(res, "api's healthcheck sets no test") {
		t.Fatalf("an empty test: %+v", res.Findings)
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
// A kairos_init of another release than the bundle's boot assessment is derived from is warned
// before ship's build refuses it; the release itself is not.
func TestAnotherKairosInitReleaseIsWarned(t *testing.T) {
	t.Parallel()
	const tag = "kairos-init:" + kairos.KairosInitRelease + "@"
	root := testutil.CopyExample(t, "full")
	for _, f := range run(t, root).Findings {
		if strings.Contains(f.String(), "kairos_init") {
			t.Errorf("the release itself: %s", f)
		}
	}
	for _, other := range []string{"kairos-init:v4.3.0@", "kairos-init@"} {
		testutil.Edit(t, root, specFile, tag, other)
		res := run(t, root)
		if !slices.ContainsFunc(res.Findings, func(f Finding) bool {
			return f.Severity == Warning && strings.Contains(f.String(), "shapes.kairos.kairos_init:") && strings.Contains(f.String(), "takes kairos-init "+kairos.KairosInitRelease+" alone")
		}) {
			t.Errorf("%s: %+v", other, res.Findings)
		}
		testutil.Edit(t, root, specFile, other, tag)
	}
}

// A base whose repository is not ubuntu is warned before ship's build refuses it; Ubuntu itself,
// from a mirror included, is not.
func TestABaseOtherThanUbuntuIsWarned(t *testing.T) {
	t.Parallel()
	const base = "base: ubuntu:26.04@"
	warned := func(res *Result) bool {
		return slices.ContainsFunc(res.Findings, func(f Finding) bool {
			return f.Severity == Warning && strings.Contains(f.String(), "shapes.kairos.base:") && strings.Contains(f.String(), "takes Ubuntu alone")
		})
	}
	root := testutil.CopyExample(t, "full")
	for _, b := range []string{base, "base: localhost:5000/mirror/ubuntu:26.04@", "base: ubuntu@"} {
		testutil.Edit(t, root, specFile, base, b)
		if res := run(t, root); warned(res) {
			t.Errorf("%s: %+v", b, res.Findings)
		}
		testutil.Edit(t, root, specFile, b, base)
	}
	for _, b := range []string{"base: debian:13@", "base: localhost:5000/ubuntu-derived:1@"} {
		testutil.Edit(t, root, specFile, base, b)
		if res := run(t, root); !warned(res) {
			t.Errorf("%s: %+v", b, res.Findings)
		}
		testutil.Edit(t, root, specFile, b, base)
	}
}

// A host setting a network namespace keeps of its own is warned: the stack's containers miss it.
// One the namespace does not keep is not.
func TestAHostSettingOfTheNamespaceIsWarned(t *testing.T) {
	t.Parallel()
	warned := func(res *Result) bool {
		return slices.ContainsFunc(res.Findings, func(f Finding) bool {
			return f.Severity == Warning && strings.Contains(f.String(), "a namespace keeps its own")
		})
	}
	root := testutil.CopyExample(t, "full")
	if res := run(t, root); warned(res) {
		t.Errorf("vm.swappiness: %+v", res.Findings)
	}
	testutil.Edit(t, root, specFile, "    vm.swappiness: {", "    net.ipv4.tcp_keepalive_time: {")
	if res := run(t, root); !warned(res) || len(errorsOf(res)) > 0 {
		t.Errorf("net.ipv4.tcp_keepalive_time: %+v", res.Findings)
	}
	for key, want := range map[string]bool{"kernel.shmmax": true, "kernel.sem": true, "fs.mqueue.msg_max": true, "net.unix.max_dgram_qlen": true, "net.ipv4.tcp_mem": false, "net.core.rmem_default": false, "kernel.pid_max": false, "net.mptcp.enabled": true, "kernel.hostname": true, "kernel.sem_next_id": true} {
		if namespaced(key) != want {
			t.Errorf("namespaced(%s) = %v", key, !want)
		}
	}
}

func TestRefTag(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]string{
		"quay.io/kairos/kairos-init:v4.4.0-rc2@sha256:ab": "v4.4.0-rc2",
		"localhost:5000/kairos-init@sha256:ab":            "",
		"localhost:5000/kairos-init:v1":                   "v1",
		"kairos-init":                                     "",
	} {
		if got := refTag(ref); got != want {
			t.Errorf("%s: %q, want %q", ref, got, want)
		}
	}
}

// A login's password may be a secret input: one an operator gives.
func TestALoginFromASecretInput(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, specFile, "basic_auth: { user: ops, secret: OPS_PASSWORD }", "basic_auth: { user: ops, secret: SEARCH_KEY }")
	if errs := errorsOf(run(t, root)); len(errs) > 0 {
		t.Fatal(strings.Join(errs, "\n"))
	}
}

// A project that does not offer none, the mode loopback serves, owes no loopback port.
func TestNoLoopbackPortsWithoutNone(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, specFile, "tls_modes: [acme, dns-cloudflare, byo, internal, none]", "tls_modes: [acme, dns-cloudflare, byo, internal]")
	testutil.Edit(t, root, specFile, "  loopback_ports: { console: 3000, api: 9090, ingest: 8080, ops: 9091, status: 9092 }\n", "")
	testutil.Edit(t, root, specFile, "  loopback_env: { COOKIE_SECURE: \"false\", TRUSTED_PROXY_HOPS: \"0\" }\n", "")
	if errs := errorsOf(run(t, root)); len(errs) > 0 {
		t.Fatal(strings.Join(errs, "\n"))
	}
}

// https_port 80 is refused where a TLS front publishes it, and is nothing to a project offering
// loopback alone.
func TestHTTPSOn80OnlyWhereATLSFrontServes(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, specFile, "  loopback_ports:", "  https_port: 80\n  loopback_ports:")
	testutil.Edit(t, root, specFile, "tls_modes: [acme, byo, internal, none]", "tls_modes: [none]")
	if errs := errorsOf(run(t, root)); len(errs) > 0 {
		t.Fatal(strings.Join(errs, "\n"))
	}
}

// A service waiting for a one-shot to complete is warned unless Compose restarts it when a re-run
// makes the one-shot again (restart: true).
func TestAOneShotsReaderRestarts(t *testing.T) {
	t.Parallel()
	const without, unknown = "services.ingest.depends_on.echo-check: waits for the one-shot echo-check without restart: true",
		"services.ingest.depends_on.echo-check: restart: ${RESTART_DEPS:-true}, which only a run knows"
	for restart, warning := range map[string]string{"": without, ", restart: false": without, ", restart: true": "", `, restart: "${RESTART_DEPS:-true}"`: unknown} {
		root := testutil.CopyExample(t, "full")
		testutil.Edit(t, root, "deploy/compose.yaml", "      - spool:/var/spool/atlas\n", "      - spool:/var/spool/atlas\n    depends_on:\n      echo-check: { condition: service_completed_successfully"+restart+" }\n")
		res := run(t, root)
		got := slices.ContainsFunc(res.Findings, func(f Finding) bool { return f.Severity == Warning && strings.Contains(f.String(), warning) })
		if warning == "" && len(res.Findings) != 0 || warning != "" && (!got || len(res.Findings) != 1) {
			t.Errorf("%q: %+v", restart, res.Findings)
		}
	}
}

func TestL4UDPOnTheFrontsPort(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, specFile, "protocol: udp, port: 17007, upstream: echo:17007", "protocol: udp, port: 443, upstream: echo:443")
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

// An L4 route's allowlist, and the front's or a route's interface, without a kairos shape are
// warned: nothing enforces them.
func TestL4AllowlistWithoutKairosWarns(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, specFile, "  kairos:\n    base: ubuntu:26.04@sha256:c098726dd9b9733f3e885f8de0ef375206a311b0a317eadc40d347b1115fb944\n    kairos_init: quay.io/kairos/kairos-init:v4.4.0-rc2@sha256:ac885c9f3002a3f0e7000947e3d1e2a5c95f5e2d0212d5f9bbd663347f1ebe0c\n    disk_formats: [raw, qcow2, ova, vhd, gce, iso]\n    cloud_publish: [aws, azure, gcp]\n    health_gate: { deadline_s: 600, deadline_after_reset_s: 1800, poll_s: 30 }\n    host_api: [api]\n    "+vmSizeLine+"   # vm create's least and default size\n", "")
	res := run(t, root)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("errors:\n%s", strings.Join(errs, "\n"))
	}
	// The front's and a route's interface too: only the VM's firewall opens a port on one alone.
	var got []string
	for _, f := range res.Findings {
		if f.Severity != Warning {
			t.Errorf("not a warning: %s", f)
		}
		got = append(got, f.String())
	}
	for _, want := range []string{
		"l4.routes[1].allow_input: only the VM's firewall enforces it, and the project has no kairos shape: the route is open to every source",
		"front.interface: only the VM's firewall opens a port on one interface alone",
		"l4.routes[1].interface: only the VM's firewall opens a port on one interface alone",
	} {
		if !slices.ContainsFunc(got, func(g string) bool { return strings.Contains(g, want) }) {
			t.Errorf("no %q in %q", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("findings: %q", got)
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

// A service given the host's powers is warned, each key on its own, never refused.
func TestHostPrivilegesWarn(t *testing.T) {
	t.Parallel()
	const img, vol = "    image: ${STOWAGE_IMAGE_SERVER}\n", "      - state:/state\n"
	for _, c := range []struct{ old, add, want string }{
		{img, "    privileged: true\n", "services.server.privileged: true: the container holds every power of the host's root"},
		{img, "    cap_add: [NET_ADMIN, SYS_TIME]\n", "services.server.cap_add: NET_ADMIN, SYS_TIME: powers of the host's root beyond Docker's defaults"},
		{img, "    pid: host\n", "services.server.pid: host: the service sees and signals every process of the host"},
		{vol, "      - /var/run/docker.sock:/var/run/docker.sock:ro\n", "services.server.volumes: the Docker socket is mounted"},
		{vol, "      - { type: bind, source: /run/docker.sock, target: /d.sock }\n", "services.server.volumes: the Docker socket is mounted"},
		{vol, "      - { source: /var/run/docker.sock, target: /d.sock }\n", "services.server.volumes: the Docker socket is mounted"},
		{vol, "      - /var/run:/host/run:ro\n", "services.server.volumes: the Docker socket is mounted"},
		{vol, "      - /:/host:ro\n", "services.server.volumes: the Docker socket is mounted"},
		{vol, "      - ${SOCK:-/var/run/docker.sock}:/var/run/docker.sock\n", "services.server.volumes: the Docker socket is mounted"},
		{img, "    privileged: ${PRIVILEGED:-true}\n", "services.server.privileged: ${PRIVILEGED:-true}, which only a run knows"},
		{img, "    privileged: \"1\"\n", "services.server.privileged: 1, which only a run knows or Compose does not read as a boolean"},
		{img, "    privileged: \"Yes\"\n", "services.server.privileged: true"},
		{img, "    privileged: \"on\"\n", "services.server.privileged: true"},
	} {
		root := testutil.CopyExample(t, "stateless")
		testutil.Edit(t, root, "deploy/compose.yaml", c.old, c.old+c.add)
		res := run(t, root)
		if len(res.Findings) != 1 || res.Findings[0].Severity != Warning || !strings.Contains(res.Findings[0].String(), c.want) {
			t.Errorf("%q: findings %+v", c.add, res.Findings)
		}
	}
	// What a service extends is its own: the base and the service are both warned.
	root := testutil.CopyExample(t, "stateless")
	testutil.Edit(t, root, "deploy/compose.yaml", img, img+"    pid: host\n    cap_add: [NET_ADMIN]\n")
	testutil.Edit(t, root, "deploy/compose.yaml", "\nconfigs:\n", "  relay:\n    extends: server\n\nconfigs:\n")
	var got []string
	for _, f := range run(t, root).Findings {
		got = append(got, f.String())
	}
	for _, want := range []string{"services.relay.pid: host", "services.relay.cap_add: NET_ADMIN", "services.server.pid: host"} {
		if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, want) }) {
			t.Errorf("no %q in %q", want, got)
		}
	}
	// What gives none: privileged false, another socket.
	root = testutil.CopyExample(t, "stateless")
	testutil.Edit(t, root, "deploy/compose.yaml", img, img+"    privileged: \"off\"\n")
	testutil.Edit(t, root, "deploy/compose.yaml", "  server:\n", "  other:\n    image: ${STOWAGE_IMAGE_SERVER}\n    privileged: false\n  server:\n")
	testutil.Edit(t, root, "deploy/compose.yaml", vol, vol+"      - /run/other.sock:/run/other.sock\n      - /var/lib/data:/data\n      - /var/run/docker.sock\n      - ${DATA:-/srv/data}:/data2\n")
	if res := run(t, root); len(res.Findings) != 0 {
		t.Errorf("findings %+v", res.Findings)
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

// An L4 route's bind address is an IP address, never a host name, a port or a zone.
func TestCheckL4Bind(t *testing.T) {
	t.Parallel()
	for v, ok := range map[string]bool{
		"": true, "192.0.2.7": true, "2001:db8::7": true, "0.0.0.0": true, "::": true,
		"localhost": false, "192.0.2.7:443": false, "[2001:db8::7]": false, "fe80::1%eth0": false, "10.0.0.300": false, " 192.0.2.7": false,
	} {
		if err := CheckL4Bind(v); (err == nil) != ok {
			t.Errorf("%q: %v", v, err)
		}
	}
}

// Whatever it accepts is written literally into the compose file Compose interpolates, and
// dialed: hex digits, dots and colons alone.
func FuzzCheckL4Bind(f *testing.F) {
	for _, s := range []string{"192.0.2.7", "::1", "fe80::1%eth0", "${X}", "1.2.3.4:5"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		if CheckL4Bind(v) != nil {
			return
		}
		if strings.Trim(v, "0123456789abcdefABCDEF.:") != "" {
			t.Fatalf("%q accepted", v)
		}
	})
}

// memShares is examples/full's services block, which gives two services a memory share.
const memShares = `services:                                                      # what a compose service asks of the host, beyond its compose file
  postgres: { memory: { percent: 25, max: 16G } }              # percent of the host's RAM, recomputed at each start, capped at 16 GiB
  api: { memory: 10, update: start-first, update_signal: SIGUSR2, update_socket: /run/atlas/handoff }  # its new container healthy beside the old before the old stops; the old one told to go once the new one runs
`

// servicesBlock is examples/full's whole services block: its memory shares, then the services' cores,
// nftables table and address of their own.
const servicesBlock = memShares + `  echo: { cpus: 1, irq: on }                                   # a whole core of its own, the NICs' interrupts on it too
  ingest: { nftables: deploy/ingest.nft }                      # its own nftables table, loaded on the VM
  relay: { interface: edge, address_input: RELAY_ADDRESS }     # an address of its own on the edge NIC (an ipvlan network)
`

// vmSizeLine is examples/full's shapes.kairos.size.
const vmSizeLine = "size: { min: { cpus: 2, memory: 2G, disk: 20G }, recommended: { cpus: 4, memory: 8G, disk: 40G } }"

// The memory shares at their bounds are clean: 1 % and 90 % each, 90 % together (the errors
// past them are TestEachRuleFiresOnFull's).
func TestMemorySharesBounds(t *testing.T) {
	t.Parallel()
	for _, shares := range []string{"services:\n  postgres: { memory: 1 }\n", "services:\n  postgres: { memory: 90 }\n", "services:\n  postgres: { memory: 60 }\n  api: { memory: 30 }\n"} {
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
		{memShares, "services:\n  api: { memory: { percent: 10, max: 64M } }\n"},
		{memShares, "services:\n  postgres: { memory: { percent: 25, max: 256M } }\n"},
		{memShares, "services:\n  api: { memory: { percent: 10, max: 256M } }\n", "  api:\n    image: ${STOWAGE_IMAGE_API}\n", "  api:\n    deploy: { replicas: \"4\" }\n    image: ${STOWAGE_IMAGE_API}\n"},
		{memShares, "services:\n  postgres: { memory: { percent: 25, max: 512M } }\n", "    mem_limit: 256m ", "    scale: 2\n    mem_limit: 256m "},
		{memShares, "services:\n  postgres: { memory: { percent: 25, max: 1536M } }\n", "    mem_limit: 256m ", "    mem_reservation: 1.5g\n    mem_limit: 256m "},
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
			if f.Severity == Warning && f.Where == "services.postgres.memory.max" {
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
		testutil.Edit(t, root, specFile, memShares, "services:\n  postgres: { memory: "+share+" }\n")
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

// The chart sizes its pods: memory shares on Kubernetes alone are warned.
func TestMemorySharesOnKubernetesAloneWarn(t *testing.T) {
	t.Parallel()
	warned := func(res *Result) bool {
		for _, f := range res.Findings {
			if f.Severity == Warning && f.Where == "services" {
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

// A project naming a tun device alone names no primary NIC: its VMs keep their one NIC as it is.
func TestATunDeviceAloneNeedsNoPrimary(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	raw, err := os.ReadFile(filepath.Join(root, specFile))
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, "host:\n  interfaces:\n    vpn0: { kind: tun, reason: \"a tunnel its service opens\" }\n"...)
	if err := os.WriteFile(filepath.Join(root, specFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if errs := errorsOf(run(t, root)); len(errs) > 0 {
		t.Errorf("errors: %v", errs)
	}
}

// A service of the project's name has no table of its own: it would be the VM's firewall's.
func TestAServiceTableOfTheProjectsNameIsRefused(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	if err := os.WriteFile(filepath.Join(root, "notes.nft"), []byte("table inet notes {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, specFile))
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, "services:\n  notes: { nftables: notes.nft }\n"...)
	if err := os.WriteFile(filepath.Join(root, specFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	errs := errorsOf(run(t, root))
	if !slices.ContainsFunc(errs, func(e string) bool {
		return strings.Contains(e, "services.notes.nftables: the service notes has the project's name")
	}) {
		t.Errorf("errors: %v", errs)
	}
}

// nft's lexing: a brace or a # in a string is the string's, a comment holds nothing; a name of a
// word nft has (exactly), a chain hooking the input with a drop policy is warned.
func TestNftTablesAsNftReadsThem(t *testing.T) {
	t.Parallel()
	ok := "table inet svc {\n\tset s { type ipv4_addr; comment \"a } # include\" } # a { comment\n\tchain c { type filter hook prerouting priority 0; policy accept; } # ruleset\n}\n"
	if err := CheckNftTable(ok, "svc"); err != nil {
		t.Errorf("clean: %v", err)
	}
	for text, want := range map[string]string{
		"table inet svc { comment \"x }":                          "a string is not closed",
		"table inet svc { }\ntable inet other { }":                "holds something after its table",
		"table inet svc { chain c { } ":                           "its braces do not balance",
		"table ip svc { }":                                        "does not start with",
		"table inet svc { chain c { jump x; } }\nflush ruleset\n": "holds something after its table",
		"table inet svc {\r\n}\r\n":                               "holds a carriage return",
	} {
		if err := CheckNftTable(text, "svc"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", text, err, want)
		}
	}
	if err := CheckNftTable("table inet \\\nsvc { chain c { } }\\\n", "svc"); err != nil {
		t.Errorf("a continuation: %v", err)
	}
	if !nftDropsInput("table inet svc { chain g { type filter hook ingress device \"eth0\" priority 0; policy drop; } }") {
		t.Error("ingress drop")
	}
	if !nftDropsInput("table inet svc { chain i { type filter hook input priority 0; policy drop; } }") ||
		nftDropsInput("table inet svc { chain f { type filter hook forward priority 0; policy drop; } chain i { type filter hook input priority 0; policy accept; } }") {
		t.Error("nftDropsInput")
	}
	if !spec.NftKeyword("mark") || spec.NftKeyword("Mark") || spec.NftKeyword("ingest") || spec.NftTableName.MatchString("a/b") {
		t.Error("names")
	}
}
