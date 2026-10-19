package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/cloud"
	"github.com/norvexlabs/stowage/internal/install"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/publish"
	"github.com/norvexlabs/stowage/internal/spec"
)

// clouds are the settings of each cloud of cloud_publish (nil for one without any) and, when one
// has some, the uploader that registers its images, working under cache: refused when tofu is not
// on PATH, which ship and promote check before anything is built.
func (o *options) clouds(p *project.Project, cache string) (publish.CloudUploader, map[string]map[string]string, error) {
	up, vars := o.cloud, map[string]map[string]string{}
	for _, c := range p.Spec.Shapes.Kairos.CloudPublish {
		v, err := cloud.Vars(c, o.getenv)
		if err != nil {
			return nil, nil, err
		}
		vars[c] = v
		if v == nil || up != nil {
			continue
		}
		if _, err := exec.LookPath("tofu"); err != nil {
			return nil, nil, fmt.Errorf("cloud %s: its settings are set, and OpenTofu registers its image: tofu is not on PATH (opentofu.org)", c)
		}
		up = &cloud.Uploader{WorkDir: filepath.Join(cache, "stowage", "cloud"), Env: cloudEnv(p), Log: o.err, Tofu: runTofu}
	}
	return up, vars, nil
}

// cloudEnv is OpenTofu's environment when it registers cloud images: the publisher's, less what
// only Stowage reads (STOWAGE_*, the registry accounts' tokens), cosign's secrets (COSIGN_*, an
// env:// signing key, Vault's VAULT_* for a hashivault:// one), which a cloud's provider never needs, and OpenTofu's own settings (TF_*:
// a data directory, extra arguments or a workspace would change what the module does).
func cloudEnv(p *project.Project) []string {
	hide := map[string]bool{}
	for _, c := range []spec.Provider{p.Spec.Registry.Push, p.Spec.Registry.Pull} {
		if c.Name == "token" {
			if t, err := providers.Token(c); err == nil {
				hide[t.TokenEnv] = true
			}
		}
	}
	vault := false
	if s := p.Spec.Signing; s != nil && s.Name == "cosign-key" {
		if c, err := providers.CosignKey(s); err == nil {
			if v, ok := strings.CutPrefix(c.Key, "env://"); ok {
				hide[v] = true
			}
			vault = strings.HasPrefix(c.Key, "hashivault://")
		}
	}
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return hide[k] || strings.HasPrefix(k, "STOWAGE_") || strings.HasPrefix(k, "COSIGN_") || strings.HasPrefix(k, "TF_") ||
			vault && strings.HasPrefix(k, "VAULT_")
	})
}

// runTofu runs the tofu on PATH as `vm create` does: in a session of its own, a cancelled run
// interrupted once (OpenTofu writes its state before it exits) and killed with what it started
// once the grace period has passed.
func runTofu(ctx context.Context, args, env []string, log io.Writer) ([]byte, error) {
	return install.ExecTool{Bin: "tofu", Grace: 5 * time.Minute}.Run(ctx, install.Cmd{Args: args, Env: env, Stream: log})
}
