package registry

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// WriteDockerConfig writes a private Docker client config.json into dir (mode 0600): c's
// credentials for reg, under every key the Docker CLI reads them by (Docker Hub's is
// authn.DefaultAuthKey), plus extra top-level fields (e.g. currentContext). Because it holds auths,
// the CLI does not fall back to a credential store, so nothing is read from or written to the
// user's own.
func WriteDockerConfig(dir string, reg name.Registry, c *authn.AuthConfig, extra map[string]any) error {
	auths := map[string]map[string]string{}
	if c != nil {
		e := map[string]string{}
		switch {
		case c.Username != "" || c.Password != "":
			e["auth"] = base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
		case c.Auth != "":
			e["auth"] = c.Auth
		}
		if c.IdentityToken != "" {
			e["identitytoken"] = c.IdentityToken
		}
		if c.RegistryToken != "" {
			e["registrytoken"] = c.RegistryToken
		}
		if len(e) > 0 {
			auths[reg.RegistryStr()] = e
			if reg.RegistryStr() == name.DefaultRegistry {
				auths[authn.DefaultAuthKey] = e
			}
		}
	}
	doc := map[string]any{"auths": auths}
	for k, v := range extra {
		doc[k] = v
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600)
}
