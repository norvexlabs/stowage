package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

// Where an install runs, for what its end prints: the commands differ per shape.
const (
	onDocker = "docker"
	onK8s    = "kubernetes"
	onVM     = "kairos"
)

// extensionCheckTime bounds the registry check of the extensions' releases, all of them: a tip
// never holds an install that finished.
var extensionCheckTime = 5 * time.Second

// Finished prints what a Docker install, re-run or upgrade in O.Dir ends with (shape onDocker, or
// onVM on the VM): the project's endpoint, when it declares one, and a tip per extension whose
// input is empty. Nothing for a dry run or a directory holding no install; it never fails.
func (in *Installer) Finished(ctx context.Context, shape string) {
	if in.O.DryRun {
		return
	}
	env, err := ReadEnvFile(in.path(envName))
	if err != nil {
		return
	}
	// The VM's watch is its host daemon's; a Docker install's is a timer of its own.
	if shape == onDocker && !in.O.Preloaded {
		if err := in.InstallWatch(ctx); err != nil {
			in.O.logf("the disk-pressure watch could not be installed: %v", err)
		}
		if err := in.InstallSchedule(ctx); err != nil {
			in.O.logf("the scheduled backup's timer could not be installed: %v", err)
		}
	}
	if url := in.dockerEndpoint(env); url != "" {
		if mode, _ := env.Get("STOWAGE_TLS_MODE"); mode == Loopback {
			fmt.Fprintf(in.O.Out, "endpoint  %s (loopback: this machine only; another compose project's containers cannot reach it, a project using %s as an extension reaches it through a domain: a new install with --tls)\n", url, in.R.Spec.Name)
		} else {
			fmt.Fprintf(in.O.Out, "endpoint  %s (the URL a project using %s as an extension is given)\n", url, in.R.Spec.Name)
		}
	}
	if shape == onDocker && !in.O.Preloaded {
		in.tuneHint(ctx)
	}
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	in.extensionTips(ctx, shape, prefix, func(key string) (string, bool) { return env.Get(key) })
}

// dockerEndpoint is the URL the install serves the endpoint route at, as it records it: the
// route's domain with the front's scheme, or its loopback URL. "" when the spec names none.
func (in *Installer) dockerEndpoint(env *EnvFile) string {
	if in.R.Spec.Endpoint == "" {
		return ""
	}
	v, _ := env.Get(spec.URLVar(in.R.Spec.Endpoint))
	return v
}

// k8sEndpoint is the URL the endpoint route is served at inside the cluster: the route's upstream
// <service>:<port>, which the chart exposes as the Service <release>-<service> on that port, and
// its path (the upstream gets it, as through the front). ""
// when the spec names no endpoint.
func (in *Installer) k8sEndpoint() string {
	s, k := in.R.Spec, in.O.K8s
	for _, r := range s.Front.Routes {
		if r.Name != s.Endpoint || s.Endpoint == "" {
			continue
		}
		svc, port, ok := strings.Cut(r.Upstream, ":")
		if !ok {
			return ""
		}
		return fmt.Sprintf("http://%s-%s.%s.svc:%s%s", k.Release, svc, k.Namespace, port, r.Path)
	}
	return ""
}

// extensionTips prints, for each extension whose input this shape records empty (value gives a
// recorded input; false when it is not one of this shape's), how to install it and how to give
// the host its endpoint afterwards. The list is the spec's; whether the registry at prefix
// publishes each on the install's channel is asked within extensionCheckTime, and only an answer
// that it does not is said.
func (in *Installer) extensionTips(ctx context.Context, shape, prefix string, value func(string) (string, bool)) {
	var off []spec.Extension
	for _, e := range in.R.Spec.Extensions {
		if v, ok := value(e.Input); ok && v == "" {
			off = append(off, e)
		}
	}
	if len(off) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, extensionCheckTime)
	defer cancel()
	channel := in.R.Manifest.Channel
	for _, e := range off {
		tag, published := in.extensionTag(ctx, prefix, e.Name, channel)
		w := in.O.Out
		fmt.Fprintf(w, "tip: the extension %s (%s) is off: %s is empty\n", e.Name, e.Prompt, e.Input)
		if !published {
			fmt.Fprintf(w, "  %s/%s-installer is not published on the %s channel\n", prefix, e.Name, channel)
		}
		get := fmt.Sprintf("docker run --rm %s/%s-installer:%s > %s.sh", prefix, e.Name, tag, e.Name)
		// The endpoint is a --set; the key, a secret input, never is: an --answers file on a re-run
		// (which reads no environment), the environment on a Kubernetes install.
		set := "--set " + e.Input + "=<its endpoint>"
		key := ""
		if e.KeyInput != "" {
			set += " --answers <a file: " + e.KeyInput + ": <its key>>"
			key = e.KeyInput + "=<its key> "
		}
		switch shape {
		case onK8s:
			fmt.Fprintf(w, "  install it: %s && sh %s.sh k8s install\n", get, e.Name)
			// An upgrade keeps a recorded input: the endpoint is taken at an install.
			fmt.Fprintf(w, "  then give its endpoint: the release takes it at a new install: `helm uninstall %s --namespace %s` (its Secret is kept), then %s%s --set %s=<its endpoint>\n",
				in.O.K8s.Release, in.O.K8s.Namespace, key, in.k8sCtl("install"), e.Input)
		case onVM:
			fmt.Fprintf(w, "  install it: %s && sh %s.sh (Docker) or sh %s.sh vm create (a VM)\n", get, e.Name, e.Name)
			fmt.Fprintf(w, "  then give its endpoint: sudo %s install %s\n", in.R.Ctl(), set)
		default:
			fmt.Fprintf(w, "  install it: %s && sh %s.sh\n", get, e.Name)
			fmt.Fprintf(w, "  then give its endpoint: %s --dir %s %s\n", in.ctl(), in.O.Dir, set)
		}
	}
}

// extensionTag is the installer tag of the extension's release on channel, as the registry at
// prefix answers: the release its channel's pointer names; latest (the bootstrap one-liner's)
// when the answer cannot be read. published is false only when the registry answers that it
// holds no such release.
func (in *Installer) extensionTag(ctx context.Context, prefix, name, channel string) (tag string, published bool) {
	if in.O.Offline || channel == "" || prefix == "" {
		return "latest", true
	}
	c, err := in.registryClient(prefix)
	if err != nil {
		return "latest", true
	}
	m, _, err := readRelease(ctx, c, prefix+"/"+name+"-release", channel)
	switch {
	case err != nil:
		return "latest", true
	case m == nil:
		return "latest", false
	case m.Name != name || m.Channel != channel:
		return "latest", true // the tag holds another project's release: no answer
	}
	if _, err := c.Tag(prefix+"/"+name+"-installer", m.Tag); err != nil {
		return "latest", true
	}
	return m.Tag, true
}
