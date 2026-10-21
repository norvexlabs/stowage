// Package providers declares the configuration of each runtime-side provider (registry auth,
// signing, health, schema compatibility, provision, backup) and validates it. The behaviour lives
// in the runtime pieces; the publisher only needs to know a spec is well-formed.
package providers

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/norvexlabs/stowage/internal/spec"
)

// EnvName is the grammar of an environment variable Stowage reads or writes.
var EnvName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// RuntimeCommand is the `command` provider on the target side: a command run inside one of the
// project's images (named by its `images` key), speaking the JSON stdin/stdout contract.
type RuntimeCommand struct {
	Image    string   `yaml:"image"`
	Cmd      []string `yaml:"cmd"`
	TimeoutS int      `yaml:"timeout_s"`
}

func (c RuntimeCommand) check(images map[string]spec.Image) error {
	if c.Image == "" {
		return errors.New("image is required (an images key)")
	}
	if _, ok := images[c.Image]; !ok {
		return fmt.Errorf("image %q is not an images key", c.Image)
	}
	if len(c.Cmd) == 0 {
		return errors.New("cmd is required")
	}
	if c.TimeoutS < 0 {
		return errors.New("timeout_s must be positive")
	}
	return nil
}

// TokenConfig is the token registry-auth provider: a user, and the variable holding its password.
type TokenConfig struct {
	User     string `yaml:"user"`
	TokenEnv string `yaml:"token_env"`
}

// Token decodes and checks a token provider.
func Token(p spec.Provider) (TokenConfig, error) {
	var c TokenConfig
	if err := p.Decode(&c); err != nil {
		return c, err
	}
	if c.User == "" {
		return c, errors.New("token: user is required")
	}
	if !EnvName.MatchString(c.TokenEnv) {
		return c, fmt.Errorf("token: token_env %q is not an environment variable name", c.TokenEnv)
	}
	return c, nil
}

// DockerConfigConfig is the docker-config registry auth provider's configuration: Config is a
// Docker config directory; empty, the user's default one.
type DockerConfigConfig struct {
	Config string `yaml:"config"`
}

// DockerConfig decodes a docker-config provider.
func DockerConfig(p spec.Provider) (DockerConfigConfig, error) {
	var c DockerConfigConfig
	err := p.Decode(&c)
	return c, err
}

// RegistryAuth validates registry.pull / registry.push.
func RegistryAuth(p spec.Provider, push bool) error {
	switch p.Name {
	case "token":
		_, err := Token(p)
		return err
	case "docker-config":
		_, err := DockerConfig(p)
		return err
	case "none":
		if push {
			return errors.New("none: publishing needs credentials (token or docker-config)")
		}
		var c struct{}
		return p.Decode(&c)
	case "":
		return errors.New("provider is required (token, docker-config, none)")
	default:
		return fmt.Errorf("unknown provider %q (token, docker-config, none)", p.Name)
	}
}

// GitHubDeleteConfig is the github delete provider's configuration: API is GitHub's REST API root
// (default https://api.github.com), TokenEnv the variable holding a token with delete:packages
// (default: the push token's variable).
type GitHubDeleteConfig struct {
	API      string `yaml:"api"`
	TokenEnv string `yaml:"token_env"`
}

// GitHubDelete decodes a github delete provider, its defaults applied: push is registry.push.
func GitHubDelete(p, push spec.Provider) (GitHubDeleteConfig, error) {
	var c GitHubDeleteConfig
	if err := p.Decode(&c); err != nil {
		return c, err
	}
	if c.API == "" {
		c.API = "https://api.github.com"
	}
	if u, err := url.Parse(c.API); err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return c, fmt.Errorf("github: api %q is not an https URL", c.API)
	}
	if c.TokenEnv == "" {
		if push.Name != "token" {
			return c, errors.New("github: token_env is required when registry.push is not a token")
		}
		t, err := Token(push)
		if err != nil {
			return c, err
		}
		c.TokenEnv = t.TokenEnv
	}
	if !EnvName.MatchString(c.TokenEnv) {
		return c, fmt.Errorf("github: token_env %q is not an environment variable name", c.TokenEnv)
	}
	return c, nil
}

// RegistryDelete validates registry.delete: oci (the default) or github.
func RegistryDelete(p, push spec.Provider) error {
	switch p.Or("oci").Name {
	case "oci":
		var c struct{}
		return p.Decode(&c)
	case "github":
		_, err := GitHubDelete(p, push)
		return err
	default:
		return fmt.Errorf("unknown provider %q (oci, github)", p.Name)
	}
}

// CosignKeyConfig is the cosign-key signing provider's configuration.
type CosignKeyConfig struct {
	// Key is the private key: a file (relative to the project root), env://VAR, or a KMS/Vault URI.
	Key string `yaml:"key"`
	// PublicKeys are project files; installs trust a signature by any of them.
	PublicKeys []string `yaml:"public_keys"`
}

// CosignKey decodes and checks a cosign-key provider.
func CosignKey(p *spec.Provider) (CosignKeyConfig, error) {
	var c CosignKeyConfig
	if err := p.Decode(&c); err != nil {
		return c, err
	}
	if c.Key == "" {
		return c, errors.New("cosign-key: key is required (a file, env://VAR, hashivault://… or a KMS URI)")
	}
	if len(c.PublicKeys) == 0 {
		return c, errors.New("cosign-key: public_keys is required (the keys installs trust; list old + new while rotating)")
	}
	return c, nil
}

// Signing validates `signing`; nil means off.
func Signing(p *spec.Provider) error {
	if p == nil {
		return nil
	}
	switch p.Name {
	case "none":
		var c struct{}
		return p.Decode(&c)
	case "cosign-key":
		_, err := CosignKey(p)
		return err
	default:
		return fmt.Errorf("unknown provider %q (none, cosign-key)", p.Name)
	}
}

// HTTPProbe is the config shared by the http-json and http-status health providers.
type HTTPProbe struct {
	Service      string `yaml:"service"`
	Port         int    `yaml:"port"`
	Path         string `yaml:"path"`
	LivenessPath string `yaml:"liveness_path"`
}

func (h HTTPProbe) check() error {
	if h.Service == "" {
		return errors.New("service is required")
	}
	if h.Port < 1 || h.Port > 65535 {
		return fmt.Errorf("port %d is not a TCP port", h.Port)
	}
	if !strings.HasPrefix(h.Path, "/") {
		return fmt.Errorf("path %q must start with /", h.Path)
	}
	if h.LivenessPath != "" && !strings.HasPrefix(h.LivenessPath, "/") {
		return fmt.Errorf("liveness_path %q must start with /", h.LivenessPath)
	}
	return nil
}

// Probe is an http health provider's probe: GET Path on Service's Port is ready when it answers
// Status (http-status) or a JSON object carrying the fields Fields (http-json).
type Probe struct {
	HTTPProbe
	Status int
	Fields map[string]any
}

// HTTPHealth decodes and validates an http-json or http-status health provider.
func HTTPHealth(p spec.Provider) (Probe, error) {
	switch p.Name {
	case "http-json":
		var c struct {
			HTTPProbe `yaml:",inline"`
			Expect    map[string]any `yaml:"expect"`
		}
		if err := p.Decode(&c); err != nil {
			return Probe{}, err
		}
		if len(c.Expect) == 0 {
			return Probe{}, errors.New("http-json: expect is required (the JSON fields a ready answer carries)")
		}
		return Probe{HTTPProbe: c.HTTPProbe, Fields: c.Expect}, c.check()
	case "http-status":
		var c struct {
			HTTPProbe `yaml:",inline"`
			Expect    int `yaml:"expect"`
		}
		if err := p.Decode(&c); err != nil {
			return Probe{}, err
		}
		if c.Expect == 0 {
			c.Expect = 200
		}
		if c.Expect < 200 || c.Expect > 399 {
			return Probe{}, fmt.Errorf("http-status: expect %d is not a success status", c.Expect)
		}
		return Probe{HTTPProbe: c.HTTPProbe, Status: c.Expect}, c.check()
	}
	return Probe{}, fmt.Errorf("%q is not an http health provider", p.Name)
}

// Health validates `health` and returns the compose service it probes ("" for command).
func Health(p spec.Provider, images map[string]spec.Image) (service string, err error) {
	switch p.Name {
	case "http-json", "http-status":
		c, err := HTTPHealth(p)
		return c.Service, err
	case "command":
		var c RuntimeCommand
		if err := p.Decode(&c); err != nil {
			return "", err
		}
		return "", c.check(images)
	case "":
		return "", errors.New("provider is required (http-json, http-status, command)")
	default:
		return "", fmt.Errorf("unknown provider %q (http-json, http-status, command)", p.Name)
	}
}

// SQLCompat is the `sql` schema-compat provider: queries the installer runs in the database
// service with the migrate role.
type SQLCompat struct {
	Current      string `yaml:"current"`       // returns the schema version, a migration number
	BarrierAfter string `yaml:"barrier_after"` // $1 is a schema version; true or a description = a barrier
	// ReadsNewerLabel, on ReadsNewerImage, is "true" on an image that reads a schema newer than its own.
	ReadsNewerLabel string `yaml:"reads_newer_label"`
	ReadsNewerImage string `yaml:"reads_newer_image"`
	// Baseline is the schema epoch the release's migrations start from: a release whose baseline
	// differs from the installed one's shares no schema with it, so an upgrade or a rollback
	// between them is refused. 0 (absent) is 1.
	Baseline int `yaml:"baseline"`
}

// SQL decodes and checks a `sql` schema-compat provider.
func SQL(p spec.Provider, images map[string]spec.Image) (SQLCompat, error) {
	var c SQLCompat
	if err := p.Decode(&c); err != nil {
		return c, err
	}
	if c.Current == "" {
		return c, errors.New("sql: current is required (the query returning the schema version)")
	}
	if c.Baseline < 0 {
		return c, fmt.Errorf("sql: baseline %d: a schema baseline is 1 or more (0, or none, is 1)", c.Baseline)
	}
	if c.BarrierAfter != "" && !strings.Contains(c.BarrierAfter, "$1") {
		return c, errors.New("sql: barrier_after must take the running schema version as $1")
	}
	if (c.ReadsNewerLabel == "") != (c.ReadsNewerImage == "") {
		return c, errors.New("sql: reads_newer_label and reads_newer_image go together (the label, and the image that carries it)")
	}
	if _, ok := images[c.ReadsNewerImage]; c.ReadsNewerImage != "" && !ok {
		return c, fmt.Errorf("sql: reads_newer_image %q is not an images key", c.ReadsNewerImage)
	}
	return c, nil
}

// SchemaBaseline is the schema baseline a `database.schema_compat` declares: the `sql` provider's
// `baseline`, where 0 (absent) is the first, 1; every other provider is at baseline 1. It reads
// that one key alone, so the rest of the provider's config, valid or not, never hides it.
func SchemaBaseline(p spec.Provider) (int, error) {
	if p.Name != "sql" {
		return 1, nil
	}
	var b int
	switch v := p.Config["baseline"].(type) {
	case nil:
	case int:
		b = v
	case int64:
		b = int(v)
	case uint64:
		b = int(min(v, 1<<31))
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("database.schema_compat: baseline %v is not an integer", v)
		}
		b = int(v)
	default:
		return 0, fmt.Errorf("database.schema_compat: baseline %v is not an integer", v)
	}
	if b < 0 {
		return 0, fmt.Errorf("database.schema_compat: baseline %d: a schema baseline is 1 or more (0, or none, is 1)", b)
	}
	return max(b, 1), nil
}

// SchemaCompat validates `database.schema_compat`.
func SchemaCompat(p spec.Provider, images map[string]spec.Image) error {
	switch p.Name {
	case "sql":
		_, err := SQL(p, images)
		return err
	case "command":
		var c RuntimeCommand
		if err := p.Decode(&c); err != nil {
			return err
		}
		return c.check(images)
	case "none":
		var c struct{}
		return p.Decode(&c)
	case "":
		return errors.New("provider is required (sql, command, none)")
	default:
		return fmt.Errorf("unknown provider %q (sql, command, none)", p.Name)
	}
}

// ProvisionHook is the `container` provision hook: cmd run in a container of the release's image.
type ProvisionHook struct {
	Image string   `yaml:"image"`
	Cmd   []string `yaml:"cmd"`
	// Env are the recorded keys the hook receives (inputs, secrets, safe defaults); required, so
	// a hook holds no secret it did not ask for. The spec's linter checks each is one.
	Env *[]string `yaml:"env"`
	// Returns are keys the hook may return; it may return others too. `provision --set` sets one
	// even before the hook returned it. Decoding refuses a name listed twice, not a variable name or
	// starting STOWAGE_ or COMPOSE_; the spec's linter refuses an input, a secret, a safe default or
	// a route's domain input.
	Returns []string `yaml:"returns"`
	Outputs string   `yaml:"outputs"` // the file in the install directory the hook's outputs are copied to
}

// Keys are the recorded keys the hook declares it reads.
func (h *ProvisionHook) Keys() []string {
	if h == nil || h.Env == nil {
		return nil
	}
	return *h.Env
}

// InstallFiles are the files and directories an install directory holds besides the hidden ones,
// the ones Stowage names stowage-* and the installer itself (<short>-stow): a hook's outputs file may
// not take one. The installer's tests check every name it writes is refused.
var InstallFiles = []string{"compose.yaml", "provision.env", "snapshots", "tls"}

// Ctl is the name of the installer of the project whose short name (spec.Spec.Short) is short:
// the binary an install keeps, next to its compose.yaml.
func Ctl(short string) string { return short + "-stow" }

// DecodeProvision decodes and validates `hooks.provision` of the project whose short name is short
// (nil = no provisioning step).
func DecodeProvision(short string, p *spec.Provider, images map[string]spec.Image) (*ProvisionHook, error) {
	if p == nil {
		return nil, nil
	}
	if p.Name != "container" {
		return nil, fmt.Errorf("unknown provider %q (container)", p.Name)
	}
	var c ProvisionHook
	if err := p.Decode(&c); err != nil {
		return nil, err
	}
	if _, ok := images[c.Image]; !ok {
		return nil, fmt.Errorf("container: image %q is not an images key", c.Image)
	}
	if len(c.Cmd) == 0 {
		return nil, errors.New("container: cmd is required")
	}
	if c.Env == nil {
		return nil, errors.New("container: env is required: the keys of the inputs, secrets and safe defaults the hook reads ([] for none)")
	}
	seen := map[string]bool{}
	for _, k := range *c.Env {
		switch {
		case !EnvName.MatchString(k):
			return nil, fmt.Errorf("container: env: %q is not a variable name", k)
		case seen[k]:
			return nil, fmt.Errorf("container: env: %s is listed twice", k)
		}
		seen[k] = true
	}
	seen = map[string]bool{}
	for _, k := range c.Returns {
		switch {
		case !EnvName.MatchString(k):
			return nil, fmt.Errorf("container: returns: %q is not a variable name", k)
		case seen[k]:
			return nil, fmt.Errorf("container: returns: %s is listed twice", k)
		case strings.HasPrefix(k, "STOWAGE_") || strings.HasPrefix(k, "COMPOSE_"):
			return nil, fmt.Errorf("container: returns: %s is a value the install records (STOWAGE_*, COMPOSE_*), not one a hook returns", k)
		}
		seen[k] = true
	}
	if c.Outputs != "" && (strings.ContainsAny(c.Outputs, "/\\\x00\n\r") || strings.HasPrefix(c.Outputs, ".") ||
		strings.HasPrefix(c.Outputs, "stowage-") || slices.Contains(InstallFiles, c.Outputs) || c.Outputs == Ctl(short)) {
		return nil, fmt.Errorf("container: outputs %q must be a file name, not hidden and not one the install writes (%s, %s, stowage-*)", c.Outputs, strings.Join(InstallFiles, ", "), Ctl(short))
	}
	return &c, nil
}

// Provision validates `hooks.provision` of the project whose short name is short (nil = no
// provisioning step).
func Provision(short string, p *spec.Provider, images map[string]spec.Image) error {
	_, err := DecodeProvision(short, p, images)
	return err
}

// profileName is a backup profile's name: a backup's file name holds it between dashes.
var profileName = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// VMBackup is the backup provider the Kairos VM runs.
const VMBackup = "pg-age-rclone"

// RunsBackups reports whether s's installs run backups: a pg-age-rclone backup, with the Docker or
// the kairos shape (whose VM runs the Docker shape's install).
// Its releases carry the backup runner.
func RunsBackups(s *spec.Spec) bool {
	return (s.Shapes.Kairos != nil || s.Shapes.Docker != nil) && s.Backup != nil && s.Backup.Name == VMBackup
}

// OwnsVolumes reports whether s's installs give data volumes their uid: a data volume with a uid,
// with the Docker or the kairos shape (whose VM runs the Docker shape's install). Its releases
// carry the volume helper.
func OwnsVolumes(s *spec.Spec) bool {
	if s.Shapes.Kairos == nil && s.Shapes.Docker == nil {
		return false
	}
	for _, v := range s.Data.Volumes {
		if v.UID != nil {
			return true
		}
	}
	return false
}

// PgAgeRclone is a decoded pg-age-rclone backup: Profiles maps each profile to the tables whose
// data it keeps (nil: every table's), and Exclude a profile to the pg_dump patterns of the tables
// whose data it leaves out (their structure kept); Default is the profile a backup takes unless
// told another. Volumes are the compose volumes every backup also takes, each an archive beside
// the dump, which a restore brings back with it.
type PgAgeRclone struct {
	Profiles map[string][]string
	Exclude  map[string][]string
	Default  string
	Volumes  []string
}

// volumeKey is a compose volume's key: it names its archive in the destination.
var volumeKey = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// tablePattern is an exclude pattern: a table, or schema.table, of letters, digits and '_', with
// pg_dump's wildcards * and ?.
var tablePattern = regexp.MustCompile(`^[A-Za-z0-9_*?]+(\.[A-Za-z0-9_*?]+)?$`)

// DecodeBackup decodes and validates a pg-age-rclone `backup`.
func DecodeBackup(p *spec.Provider) (*PgAgeRclone, error) {
	if p == nil || p.Name != VMBackup {
		return nil, errors.New("backup: not pg-age-rclone")
	}
	var c struct {
		DefaultProfile string         `yaml:"default_profile"`
		Profiles       map[string]any `yaml:"profiles"`
		Volumes        []string       `yaml:"volumes"`
	}
	if err := p.Decode(&c); err != nil {
		return nil, err
	}
	for i, v := range c.Volumes {
		if !volumeKey.MatchString(v) {
			return nil, fmt.Errorf("pg-age-rclone: volumes[%d] %q is not a compose volume's key", i, v)
		}
		if slices.Contains(c.Volumes[:i], v) {
			return nil, fmt.Errorf("pg-age-rclone: volumes[%d] %q is listed twice", i, v)
		}
	}
	if len(c.Profiles) == 0 {
		return nil, errors.New("pg-age-rclone: profiles is required (e.g. { full: all })")
	}
	out := &PgAgeRclone{Profiles: map[string][]string{}, Exclude: map[string][]string{}, Default: c.DefaultProfile, Volumes: c.Volumes}
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !profileName.MatchString(name) {
			return nil, fmt.Errorf("pg-age-rclone: profile %q must be lowercase letters and digits (it names the backup's files)", name)
		}
		switch t := c.Profiles[name].(type) {
		case string:
			if t != "all" {
				return nil, fmt.Errorf("pg-age-rclone: profile %s must be \"all\", a table list, or { exclude: [pattern, …] }", name)
			}
			out.Profiles[name] = nil
		case []any:
			if len(t) == 0 {
				return nil, fmt.Errorf("pg-age-rclone: profile %s lists no table", name)
			}
			tables := []string{}
			for _, tbl := range t {
				s, ok := tbl.(string)
				if !ok || s == "" {
					return nil, fmt.Errorf("pg-age-rclone: profile %s has a non-string table", name)
				}
				tables = append(tables, s)
			}
			out.Profiles[name] = tables
		case map[string]any:
			ex, ok := t["exclude"].([]any)
			if len(t) != 1 || !ok || len(ex) == 0 {
				return nil, fmt.Errorf("pg-age-rclone: profile %s must be \"all\", a table list, or { exclude: [pattern, …] }", name)
			}
			for _, e := range ex {
				s, ok := e.(string)
				if !ok || !tablePattern.MatchString(s) {
					return nil, fmt.Errorf("pg-age-rclone: profile %s: exclude %v is not a table pattern (table or schema.table; letters, digits, _, * and ?)", name, e)
				}
				out.Exclude[name] = append(out.Exclude[name], s)
			}
			out.Profiles[name] = nil
		default:
			return nil, fmt.Errorf("pg-age-rclone: profile %s must be \"all\", a table list, or { exclude: [pattern, …] }", name)
		}
	}
	if c.DefaultProfile != "" {
		if _, ok := c.Profiles[c.DefaultProfile]; !ok {
			return nil, fmt.Errorf("pg-age-rclone: default_profile %q is not a profile", c.DefaultProfile)
		}
	} else if len(c.Profiles) > 1 {
		return nil, errors.New("pg-age-rclone: default_profile is required when there are several profiles")
	} else {
		out.Default = names[0]
	}
	return out, nil
}

// Backup validates `backup` (nil = no backup on the shapes that keep data locally).
func Backup(p *spec.Provider, images map[string]spec.Image) error {
	if p == nil {
		return nil
	}
	switch p.Name {
	case VMBackup:
		_, err := DecodeBackup(p)
		return err
	case "command":
		var c RuntimeCommand
		if err := p.Decode(&c); err != nil {
			return err
		}
		return c.check(images)
	default:
		return fmt.Errorf("unknown provider %q (pg-age-rclone, command)", p.Name)
	}
}

// SecretGenerators are the generators a `secrets` entry may name.
var SecretGenerators = map[string]string{
	"hex32":  "32 random bytes, hex (64 characters)",
	"hex16":  "16 random bytes, hex (32 characters)",
	"pass18": "18 random alphanumeric characters",
	"pass32": "32 random alphanumeric characters",
	"b64-32": "32 random bytes, base64url without padding",
}

// TLSModes are the front's certificate sources.
var TLSModes = []string{"acme", "dns-cloudflare", "byo", "internal", "none"}
