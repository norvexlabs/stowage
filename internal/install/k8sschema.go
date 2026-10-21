package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/sigverify"
)

// The schema check on Kubernetes (shapes.kubernetes.schema): the sql schema-compat queries run
// with psql in a pod of the release's namespace, made from the pod template of the workload
// running reads_newer_image — its environment, its pull Secrets, its service account — with
// the schema image and psql in place of its container. `k8s upgrade` records the schema it read
// before helm migrates (the values' stowage.schemaFrom); `k8s rollback` refuses as Docker's
// rollback does: a barrier since that schema, or a release that does not read the newer one.

// k8sSchemaFrom is the values key under helm.ValuesKey recording the schema an upgrade read
// before it migrated.
const k8sSchemaFrom = "schemaFrom"

// Bounds on one schema pod: its whole run, and how often it is looked at.
var (
	k8sSchemaTimeout = 3 * time.Minute
	k8sSchemaPoll    = time.Second
)

// k8sSchemaOn reports whether the release checks schemas on Kubernetes.
func (in *Installer) k8sSchemaOn() bool {
	k := in.R.Spec.Shapes.Kubernetes
	return in.schemaOn() && in.R.Spec.DB().SchemaCompat.Name == "sql" && k != nil && k.Schema != nil
}

// k8sSchema is the schema-compat provider read on Kubernetes, the images taken from registry.
func (in *Installer) k8sSchema(registry string) schema {
	return schema{in: in, sql: func(ctx context.Context, q string) (string, error) { return in.k8sPSQL(ctx, registry, q) }}
}

// k8sManifestObject is the part of a workload of the release's manifest the probe reads.
type k8sManifestObject struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec map[string]any `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// k8sTemplateOf is the pod template spec of the workload of the release running the image key
// (by its repository, whatever its tag or digest), and that container's index.
func (in *Installer) k8sTemplateOf(ctx context.Context, key string) (map[string]any, map[string]any, error) {
	k := in.O.K8s
	out, err := in.helm(ctx, Cmd{Args: []string{"get", "manifest", k.Release}})
	if err != nil {
		return nil, nil, err
	}
	repo := "/" + in.R.Spec.Name + "-" + key
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var o k8sManifestObject
		if err := dec.Decode(&o); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("helm get manifest: %w", err)
		}
		switch o.Kind {
		case "Deployment", "StatefulSet", "DaemonSet":
		default:
			continue
		}
		cs, _ := o.Spec.Template.Spec["containers"].([]any)
		for _, c := range cs {
			cm, _ := c.(map[string]any)
			img, _ := cm["image"].(string)
			name, _, _ := strings.Cut(img, "@")
			if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
				name = name[:i]
			}
			if strings.HasSuffix(name, repo) {
				return o.Spec.Template.Spec, cm, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("release %s in %s runs no workload of the image %s (database.schema_compat.reads_newer_image)", k.Release, k.Namespace, key)
}

// k8sPSQL runs one query as the migrate role, in a pod of the release, and returns its one value.
func (in *Installer) k8sPSQL(ctx context.Context, registry, q string) (string, error) {
	k, sc := in.O.K8s, in.R.Spec.Shapes.Kubernetes.Schema
	c, err := providers.SQL(in.R.Spec.DB().SchemaCompat, in.R.Spec.Images)
	if err != nil {
		return "", err
	}
	tmpl, ctr, err := in.k8sTemplateOf(ctx, c.ReadsNewerImage)
	if err != nil {
		return "", err
	}
	digest := in.R.Manifest.Artifacts.Images[sc.Image]
	if digest == "" {
		return "", fmt.Errorf("the release has no image %s (shapes.kubernetes.schema.image)", sc.Image)
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	name := k.hookJob() + "-schema-" + hex.EncodeToString(suffix)
	container := map[string]any{
		"name":    "schema",
		"image":   registry + "/" + in.R.Spec.Name + "-" + sc.Image + "@" + digest,
		"command": []string{"psql", "-X", "-q", "-t", "-A", "-v", "ON_ERROR_STOP=1", "-c", q, "$(" + sc.URLEnv + ")"},
		"securityContext": map[string]any{
			"runAsNonRoot": true, "runAsUser": 65534, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"capabilities": map[string]any{"drop": []string{"ALL"}}, "seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"resources": map[string]any{"limits": map[string]any{"cpu": "200m", "memory": "64Mi"}},
	}
	for _, f := range []string{"env", "envFrom"} {
		if v, ok := ctr[f]; ok {
			container[f] = v
		}
	}
	spec := map[string]any{"restartPolicy": "Never", "containers": []any{container}, "automountServiceAccountToken": false}
	for _, f := range []string{"imagePullSecrets", "serviceAccountName", "nodeSelector", "tolerations"} {
		if v, ok := tmpl[f]; ok {
			spec[f] = v
		}
	}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "labels": map[string]string{
			"app.kubernetes.io/managed-by": managedBy, "app.kubernetes.io/part-of": in.R.Spec.Name, "app.kubernetes.io/instance": k.Release}},
		"spec": spec}
	body, err := json.Marshal(pod)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, k8sSchemaTimeout)
	defer cancel()
	if _, err := in.kubectl(ctx, Cmd{Args: []string{"create", "--filename", "-"}, Stdin: bytes.NewReader(body)}); err != nil {
		return "", fmt.Errorf("the schema pod: %w", err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer dcancel()
		if _, err := in.kubectl(dctx, Cmd{Args: []string{"delete", "pod", name, "--ignore-not-found", "--wait=false"}}); err != nil {
			in.O.logf("the schema pod %s could not be deleted: `kubectl delete pod %s --namespace %s` (%v)", name, name, k.Namespace, err)
		}
	}()
	for {
		out, err := in.kubectl(ctx, Cmd{Args: []string{"get", "pod", name, "--output", "jsonpath={.status.phase}"}})
		if err != nil {
			return "", fmt.Errorf("the schema pod: %w", err)
		}
		phase := strings.TrimSpace(string(out))
		if phase == "Succeeded" || phase == "Failed" {
			logs, lerr := in.kubectl(ctx, Cmd{Args: []string{"logs", name}})
			if lerr != nil {
				return "", fmt.Errorf("the schema pod's output: %w", lerr)
			}
			if phase == "Failed" {
				return "", fmt.Errorf("the schema pod failed: %s", strings.TrimSpace(string(logs)))
			}
			return strings.TrimSpace(string(logs)), nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("the schema pod %s did not finish within %s (phase %q)", name, k8sSchemaTimeout, phase)
		case <-time.After(k8sSchemaPoll):
		}
	}
}

// k8sRecordSchema is the schema an upgrade records before helm migrates: "" when the release
// does not check it or it could not be read (said in the log).
func (in *Installer) k8sRecordSchema(ctx context.Context, registry string) string {
	if !in.k8sSchemaOn() {
		return ""
	}
	v, err := in.k8sSchema(registry).current(ctx, nil)
	if err != nil {
		in.O.logf("the database's schema could not be read (%v): not recorded, so a rollback past this upgrade is not checked", err)
		return ""
	}
	in.O.logf("the database is at schema %s before the upgrade", v)
	return v
}

// k8sRollbackCheck refuses a rollback to revision to, from the release cur runs, that the
// database's schema forbids: a rollback barrier since the schema recorded by the upgrade that
// replaced revision to, or a newer schema that release's reads_newer_image does not read.
func (in *Installer) k8sRollbackCheck(ctx context.Context, cur *deployed, to int) error {
	o, k := &in.O, in.O.K8s
	if !in.schemaOn() {
		return nil
	}
	unchecked := func(why string, a ...any) error {
		o.logf("going back unchecked: "+why, a...)
		return nil
	}
	if !in.k8sSchemaOn() {
		return unchecked("%s does not set shapes.kubernetes.schema", in.R.Spec.Name)
	}
	target, err := in.revisionDeployed(ctx, to)
	if err != nil {
		return unchecked("what revision %d runs is not known (%v)", to, err)
	}
	after, err := in.revisionHistory(ctx, to+1)
	if err != nil || after == nil || rolledBackTo.MatchString(after.Description) {
		return unchecked("the schema before the upgrade from revision %d was not recorded", to)
	}
	var v struct {
		Stowage map[string]any `json:"stowage"`
	}
	raw, err := in.revisionValues(ctx, to+1)
	if err == nil {
		err = json.Unmarshal(raw, &v)
	}
	since, _ := v.Stowage[k8sSchemaFrom].(string)
	if err != nil || !schemaVersion.MatchString(since) {
		return unchecked("the schema before the upgrade from revision %d was not recorded", to)
	}
	s := in.k8sSchema(cmp.Or(k.Registry, cur.Registry))
	now, err := s.current(ctx, nil)
	if err != nil {
		return unchecked("the schema version could not be read (%v)", err)
	}
	if !laterSchema(now, since) {
		return nil
	}
	this := fmt.Sprintf("%s %s (revision %d)", target.Release.Name, target.Release.Version, target.Release.Revision)
	refuse := func(why string) error {
		return rollbackRefused(fmt.Sprintf("refusing: %s; going back to revision %d would leave the release unable to start.", why, to))
	}
	barriers, err := s.barriers(ctx, since)
	if err != nil {
		o.logf("the rollback barriers could not be read (%v)", err)
	} else if barriers != "" {
		return refuse(fmt.Sprintf("the database carries %s, which removed something %s may still use", barriers, this))
	}
	reads, err := in.k8sReadsNewer(ctx, cmp.Or(k.Registry, target.Registry), target)
	if err != nil {
		return unchecked("whether %s reads the newer schema %s is not known (%v)", this, now, err)
	}
	if !reads {
		return refuse(fmt.Sprintf("the database is at %s, and %s predates reading a newer schema", now, this))
	}
	o.logf("the database is at %s; %s reads past the newer migrations", now, this)
	return nil
}

// k8sReadsNewer reports whether the release d recorded reads a schema newer than its own: the
// label of its reads_newer_image, read from its release manifest in the registry at prefix,
// signed by a key this installer carries.
func (in *Installer) k8sReadsNewer(ctx context.Context, prefix string, d *deployed) (bool, error) {
	c, err := providers.SQL(in.R.Spec.DB().SchemaCompat, in.R.Spec.Images)
	if err != nil {
		return false, err
	}
	if c.ReadsNewerLabel == "" {
		return false, errors.New("database.schema_compat sets no reads_newer_label")
	}
	cl, err := in.registryClient(prefix)
	if err != nil {
		return false, err
	}
	repo, err := cl.Repo(prefix + "/" + d.Release.Name + "-release")
	if err != nil {
		return false, err
	}
	m, h, err := readRelease(ctx, cl, repo.String(), d.Release.Tag)
	if err == nil && m == nil {
		err = fmt.Errorf("%s holds no release manifest", d.Release.Tag)
	}
	if err != nil {
		return false, err
	}
	var trust *sigverify.Verifier
	if trust, err = trusted("this installer", in.R.Keys); err == nil {
		err = checkSigned(ctx, cl, repo, h, m, trust)
	}
	if err != nil {
		return false, err
	}
	digest := m.Artifacts.Images[c.ReadsNewerImage]
	if digest == "" {
		return false, fmt.Errorf("%s %s has no image %s", m.Name, m.ProjectVersion, c.ReadsNewerImage)
	}
	ref, err := cl.Digest(prefix+"/"+m.Name+"-"+c.ReadsNewerImage, digest)
	if err != nil {
		return false, err
	}
	labels, err := cl.Labels(ctx, ref)
	if err != nil {
		return false, err
	}
	// The label is the image's claim: without it, the image predates reading a newer schema.
	return labels[c.ReadsNewerLabel] == "true", nil
}

// revisionDeployed is what revision rev of the release ran, from its values.
func (in *Installer) revisionDeployed(ctx context.Context, rev int) (*deployed, error) {
	raw, err := in.revisionValues(ctx, rev)
	if err != nil {
		return nil, err
	}
	var v struct {
		Stowage *deployed `json:"stowage"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if v.Stowage == nil || v.Stowage.Release.Tag == "" {
		return nil, fmt.Errorf("revision %d records no release", rev)
	}
	return v.Stowage, nil
}

// revisionHistory is revision rev of the release's helm history; nil when helm keeps none.
func (in *Installer) revisionHistory(ctx context.Context, rev int) (*helmRevision, error) {
	out, err := in.helm(ctx, Cmd{Args: []string{"history", in.O.K8s.Release, "--max", "1000", "--output", "json"}})
	if err != nil {
		return nil, err
	}
	var hs []helmRevision
	if err := json.Unmarshal(out, &hs); err != nil {
		return nil, fmt.Errorf("helm history: %w", err)
	}
	for _, h := range hs {
		if h.Revision == rev {
			return &h, nil
		}
	}
	return nil, nil
}
