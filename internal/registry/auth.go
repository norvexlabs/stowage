package registry

import (
	"fmt"
	"os"

	"github.com/docker/cli/cli/config"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// Keychain returns the credentials a registry.pull or registry.push provider names. getenv reads
// the environment (os.Getenv in production).
func Keychain(p spec.Provider, getenv func(string) string) (authn.Keychain, error) {
	switch p.Name {
	case "token":
		c, err := providers.Token(p)
		if err != nil {
			return nil, err
		}
		tok := getenv(c.TokenEnv)
		if tok == "" {
			return nil, fmt.Errorf("token: %s is not set", c.TokenEnv)
		}
		return Fixed(authn.FromConfig(authn.AuthConfig{Username: c.User, Password: tok})), nil
	case "docker-config":
		c, err := providers.DockerConfig(p)
		if err != nil {
			return nil, err
		}
		if c.Config == "" {
			return authn.DefaultKeychain, nil
		}
		return dockerConfig{dir: c.Config}, nil
	case "none":
		return Fixed(authn.Anonymous), nil
	default:
		return nil, fmt.Errorf("unknown registry auth provider %q", p.Name)
	}
}

// Fixed is a keychain that answers a with every registry.
func Fixed(a authn.Authenticator) authn.Keychain { return fixedAuth{a} }

type fixedAuth struct{ a authn.Authenticator }

func (s fixedAuth) Resolve(authn.Resource) (authn.Authenticator, error) { return s.a, nil }

// dockerConfig reads a docker config directory other than the default one.
type dockerConfig struct{ dir string }

func (d dockerConfig) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if _, err := os.Stat(d.dir); err != nil {
		return nil, fmt.Errorf("docker-config: %w", err)
	}
	cf, err := config.Load(d.dir)
	if err != nil {
		return nil, fmt.Errorf("docker-config %s: %w", d.dir, err)
	}
	keys := []string{r.String(), r.RegistryStr()}
	if r.RegistryStr() == name.DefaultRegistry {
		keys = append(keys, authn.DefaultAuthKey) // where `docker login` stores Docker Hub
	}
	for _, key := range keys {
		ac, err := cf.GetAuthConfig(key)
		if err != nil {
			return nil, fmt.Errorf("docker-config %s: %w", d.dir, err)
		}
		if ac.Username != "" || ac.Password != "" || ac.Auth != "" || ac.IdentityToken != "" || ac.RegistryToken != "" {
			return authn.FromConfig(authn.AuthConfig{
				Username: ac.Username, Password: ac.Password, Auth: ac.Auth,
				IdentityToken: ac.IdentityToken, RegistryToken: ac.RegistryToken,
			}), nil
		}
	}
	// No entry: anonymous, as docker itself does (a registry needing credentials then refuses).
	return authn.Anonymous, nil
}
