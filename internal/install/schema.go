package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/command"
	"github.com/norvexlabs/stowage/internal/providers"
)

// The `command` schema-compat provider runs as a service Stowage adds to the stack, like the
// provision hook.
const (
	SchemaService = "stowage-schema"
	SchemaCompose = "stowage-schema.yaml"
)

// Bounds on one schema command.
var (
	schemaUnit      = time.Second // timeout_s counts these
	maxSchemaAnswer = 64 << 10
)

// schemaVersion is a migration number: the only form the installer compares, puts in a query or
// sends to a command, without leading zeros and exact as a JSON number read into a float64.
var schemaVersion = regexp.MustCompile(`^(0|[1-9][0-9]{0,14})$`)

// param is a query parameter: only $1 itself is the schema version, not $10.
var param = regexp.MustCompile(`\$1[0-9]*`)

// laterSchema reports whether migration number a is after b; both match schemaVersion.
func laterSchema(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

// schema answers the schema-compat ops on an install. stack is the recorded file of the running
// stack, which a query or a command runs against.
type schema struct {
	in    *Installer
	stack *EnvFile
	// sql, when set, runs a query in its place (on Kubernetes, in a pod).
	sql func(ctx context.Context, q string) (string, error)
}

// on reports whether the release checks schemas at all.
func (in *Installer) schemaOn() bool {
	d := in.R.Spec.DB()
	return d.Service != "" && d.SchemaCompat.Name != "" && d.SchemaCompat.Name != "none"
}

// current returns the database's schema version. image is the stack image the `command` provider
// runs in, from the recorded file that names it.
func (s schema) current(ctx context.Context, image *EnvFile) (string, error) {
	p := s.in.R.Spec.DB().SchemaCompat
	var v string
	switch p.Name {
	case "sql":
		c, err := providers.SQL(p, s.in.R.Spec.Images)
		if err != nil {
			return "", err
		}
		if v, err = s.psql(ctx, c.Current); err != nil {
			return "", err
		}
	case "command":
		var out struct {
			Version json.RawMessage `json:"version"`
		}
		if err := s.call(ctx, image, map[string]any{"op": "schema.current"}, &out); err != nil {
			return "", err
		}
		v = strings.Trim(string(out.Version), `"`)
	default:
		return "", fmt.Errorf("database.schema_compat: unknown provider %q", p.Name)
	}
	if n := strings.TrimLeft(v, "0"); n != v && v != "" {
		v = cmp.Or(n, "0")
	}
	if !schemaVersion.MatchString(v) {
		return "", fmt.Errorf("schema.current: %q is not a migration number (at most 15 digits)", v)
	}
	return v, nil
}

// barriers returns the rollback barriers the database carries after migration since, described;
// empty when there is none.
func (s schema) barriers(ctx context.Context, since string) (string, error) {
	p := s.in.R.Spec.DB().SchemaCompat
	switch p.Name {
	case "sql":
		c, err := providers.SQL(p, s.in.R.Spec.Images)
		if err != nil || c.BarrierAfter == "" {
			return "", err
		}
		out, err := s.psql(ctx, param.ReplaceAllStringFunc(c.BarrierAfter, func(m string) string {
			if m == "$1" {
				return since
			}
			return m
		}))
		if err != nil {
			return "", err
		}
		switch strings.ToLower(out) {
		case "", "f", "false", "0":
			return "", nil
		case "t", "true":
			return "a rollback barrier after migration " + since, nil
		}
		return out, nil
	case "command":
		var out struct {
			Barriers *[]string `json:"barriers"`
		}
		if err := s.call(ctx, s.stack, map[string]any{"op": "schema.barrier_after", "version": json.Number(since)}, &out); err != nil {
			return "", err
		}
		if out.Barriers == nil {
			return "", errors.New(`schema.barrier_after: the answer must be {"barriers": [...]}`)
		}
		return strings.Join(*out.Barriers, ", "), nil
	}
	return "", fmt.Errorf("database.schema_compat: unknown provider %q", p.Name)
}

// readsNewer reports whether the release prev recorded reads the schema now, newer than its own.
func (s schema) readsNewer(ctx context.Context, prev *EnvFile, now string) (bool, error) {
	p := s.in.R.Spec.DB().SchemaCompat
	switch p.Name {
	case "sql":
		c, err := providers.SQL(p, s.in.R.Spec.Images)
		if err != nil {
			return false, err
		}
		if c.ReadsNewerLabel == "" {
			return false, errors.New("database.schema_compat sets no reads_newer_label")
		}
		ref, ok := prev.Get(ImageVar(c.ReadsNewerImage))
		if !ok {
			return false, fmt.Errorf("the previous release has no image %s", c.ReadsNewerImage)
		}
		out, err := s.in.O.Docker.Run(ctx, Cmd{Args: []string{"image", "inspect", "--format", "{{json .Config.Labels}}", ref}})
		if err != nil {
			return false, err
		}
		var labels map[string]string
		if err := json.Unmarshal(bytes.TrimSpace(out), &labels); err != nil {
			return false, fmt.Errorf("the labels of %s: %w", ref, err)
		}
		// The label is the image's claim: without it, the image predates reading a newer schema.
		return labels[c.ReadsNewerLabel] == "true", nil
	case "command":
		var out struct {
			ReadsNewer *bool `json:"reads_newer"`
		}
		if err := s.call(ctx, prev, map[string]any{"op": "schema.reads_newer", "version": json.Number(now)}, &out); err != nil {
			return false, err
		}
		if out.ReadsNewer == nil {
			return false, errors.New(`schema.reads_newer: the answer must be {"reads_newer": true|false}`)
		}
		return *out.ReadsNewer, nil
	}
	return false, fmt.Errorf("database.schema_compat: unknown provider %q", p.Name)
}

// psql runs a query in the database service as the migrate role and returns its one value.
func (s schema) psql(ctx context.Context, q string) (string, error) {
	if s.sql != nil {
		return s.sql(ctx, q)
	}
	d := s.in.R.Spec.DB()
	out, err := s.in.Compose(ctx, s.stack, nil, "exec", "-T", d.Service,
		"psql", "-X", "-q", "-t", "-A", "-v", "ON_ERROR_STOP=1", "-U", d.Roles.Migrate, "-d", d.Name, "-c", q)
	return strings.TrimSpace(string(out)), err
}

// call runs one op of the `command` schema-compat provider (runCommand), in the image its config
// names, taken from image.
func (s schema) call(ctx context.Context, image *EnvFile, req map[string]any, resp any) error {
	var c providers.RuntimeCommand
	if err := s.in.R.Spec.DB().SchemaCompat.Decode(&c); err != nil {
		return err
	}
	return s.in.runCommand(ctx, commandRun{what: "the schema-compat command", service: SchemaService, file: SchemaCompose,
		name: containerName(s.stack, SchemaService)}, c, image, s.stack, req, resp)
}

// commandRun is where runCommand runs a provider's command: the service it adds to the stack, in
// file (in the install directory), as the container name.
type commandRun struct {
	what, service, file, name string
}

// runCommand runs one op of a `command` provider, c, in a confined container of the image its
// config names, taken from image; the command reaches the stack on its network and receives the
// recorded values (recordedEnv), the services on the host's network by name (targetHosts). Exit 2 is
// command.ErrNoAnswer.
func (in *Installer) runCommand(ctx context.Context, r commandRun, c providers.RuntimeCommand, image, stack *EnvFile, req map[string]any, resp any) error {
	ref, ok := image.Get(ImageVar(c.Image))
	if !ok {
		return fmt.Errorf("no image %s is recorded", c.Image)
	}
	svc := confined(r.service, strings.ReplaceAll(ref, "$", "$$"), c.Cmd, in.recordedEnv(stack))
	if hosts := in.targetHosts(); len(hosts) > 0 {
		svc["extra_hosts"] = hosts
	}
	file, err := serviceFile(r.what, r.service, svc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(in.path(r.file), file, 0o644); err != nil {
		return err
	}
	defer os.Remove(in.path(r.file))
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	timeout := 30 * schemaUnit
	if c.TimeoutS > 0 {
		timeout = time.Duration(c.TimeoutS) * schemaUnit
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &capped{limit: maxSchemaAnswer}
	_, err = in.O.Docker.Run(runCtx, Cmd{
		Args:   append(in.composeBase(r.file), "run", "--rm", "--no-deps", "-T", "--name", r.name, r.service),
		Env:    stack.Environ(in.O.environ()),
		Stdin:  bytes.NewReader(body),
		Stdout: out,
	})
	op := req["op"]
	if runCtx.Err() != nil {
		rmCtx, rmCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = in.O.Docker.Run(rmCtx, Cmd{Args: []string{"rm", "--force", r.name}})
		rmCancel()
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		return fmt.Errorf("%s: stopped after %s", op, timeout)
	}
	var code interface{ ExitCode() int }
	if errors.As(err, &code) && code.ExitCode() == 2 {
		return command.ErrNoAnswer
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if out.over {
		return fmt.Errorf("%s: the answer is longer than %d bytes", op, maxSchemaAnswer)
	}
	if err := json.Unmarshal(out.buf.Bytes(), resp); err != nil {
		return fmt.Errorf("%s: the answer is not JSON: %w", op, err)
	}
	return nil
}

// capped keeps the first limit bytes written to it.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.over = true
		return len(p), nil
	}
	return c.buf.Write(p)
}
