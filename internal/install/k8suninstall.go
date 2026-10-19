package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// k8sOwned is one object the installer makes beside the helm release: its kind, as kubectl names
// it, and its name.
type k8sOwned struct{ kind, name string }

// k8sOwnedObjects are what the installer makes beside the helm release, in the order an uninstall
// deletes them: the provision Job first (its pod reads the rest), then the hook's account, then the
// Secrets.
func (k K8sOptions) k8sOwnedObjects() []k8sOwned {
	hook := k.hookName()
	return []k8sOwned{
		{"job", k.hookJob()},
		{"rolebinding", hook}, {"role", hook}, {"serviceaccount", hook},
		{"secret", hook}, {"secret", k.pullSecret()}, {"secret", k.envSecret()},
	}
}

// ownedState reads one object beside the release: whether it exists, and whether this installer
// made it for this release (its labels say so). Another tool's object of the name is never
// deleted.
func (in *Installer) ownedState(ctx context.Context, o k8sOwned) (exists, ours bool, err error) {
	out, err := in.kubectl(ctx, Cmd{Args: []string{"get", o.kind, o.name, "--ignore-not-found", "--output", "json"}})
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return false, false, err
	}
	var obj struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		return false, false, fmt.Errorf("%s %s: %w", o.kind, o.name, err)
	}
	l := obj.Metadata.Labels
	return true, l["app.kubernetes.io/managed-by"] == managedBy && l["app.kubernetes.io/instance"] == in.O.K8s.Release, nil
}

// K8sUninstall removes a helm release and what the installer made beside it: its Secrets, the
// provision hook's Secret, account and Job. The release's PersistentVolumeClaims go only with
// --data, or when the operator says so; with --yes it never asks. Data kept keeps the Secrets that
// answer for it, and a claim `helm uninstall` deletes itself is never taken without --data. It reads the release as helm and
// the labels have it, never the release this installer carries, so it removes one an older
// installer made. It prints what it kept; the namespace is always kept.
func (in *Installer) K8sUninstall(ctx context.Context) error {
	o, k, name := &in.O, in.O.K8s, in.R.Spec.Name
	if err := in.checkTarget(); err != nil {
		return err
	}
	if _, err := in.checkHelm(ctx); err != nil {
		return err
	}
	if _, err := k.Kubectl.Run(ctx, Cmd{Args: []string{"version", "--client"}}); err != nil {
		return fmt.Errorf("kubectl is required: %w", err)
	}
	rel, err := in.release(ctx)
	if err != nil {
		return err
	}
	if rel != nil && rel.Status == "uninstalled" {
		rel = nil // helm kept its history only
	}
	var claims, keptClaims, byHelm []string
	var cur *deployed
	if rel != nil {
		out, err := in.helm(ctx, Cmd{Args: []string{"get", "values", k.Release, "--all", "--output", "json"}})
		if err != nil {
			return err
		}
		var v struct {
			Stowage *deployed `json:"stowage"`
		}
		if err := json.Unmarshal(out, &v); err != nil {
			return fmt.Errorf("helm get values: %w", err)
		}
		// Any project's release Stowage published: a release of another name is how a project
		// renamed leaves its old one, and the confirmation names it.
		if v.Stowage == nil || v.Stowage.Release.Name == "" {
			return fmt.Errorf("release %s in %s was not installed from a chart Stowage published: `helm uninstall` removes it", k.Release, k.Namespace)
		}
		cur = v.Stowage
		manifest, err := in.helm(ctx, Cmd{Args: []string{"get", "manifest", k.Release}})
		if err != nil {
			return err
		}
		listed, err := in.kubectl(ctx, Cmd{Args: []string{"get", "persistentvolumeclaims", "--output", "json"}})
		if err != nil {
			return err
		}
		if claims, keptClaims, byHelm, err = releaseClaims(manifest, listed, k.Namespace); err != nil {
			return err
		}
	}
	var owned []k8sOwned
	var foreign []string
	for _, ob := range k.k8sOwnedObjects() {
		exists, ours, err := in.ownedState(ctx, ob)
		switch {
		case err != nil:
			return err
		case ours:
			owned = append(owned, ob)
		case exists:
			foreign = append(foreign, ob.kind+" "+ob.name)
		}
	}
	if rel == nil && len(owned) == 0 {
		return fmt.Errorf("no release %s in %s, and nothing %s made for it there", k.Release, k.Namespace, in.R.Ctl())
	}
	var what []string
	if rel != nil {
		r := cur.Release
		what = append(what, fmt.Sprintf("uninstall release %s in %s, which runs %s %s (revision %d)", k.Release, k.Namespace, r.Name, r.Version, r.Revision))
	}
	data := k.Data && len(claims) > 0
	asked := false
	if !k.Data && len(claims) > 0 && !o.Yes && !o.DryRun && o.Prompt != nil {
		title := fmt.Sprintf("Uninstall %s: %s. Its PersistentVolumeClaims %s hold its data", name, strings.Join(what, ", "), strings.Join(claims, ", "))
		choices := []Choice{{"no", "No, change nothing"}, {"keep", "Yes, and keep the data"}, {"data", "Yes, and delete the data"}}
		if len(byHelm) > 0 {
			title += fmt.Sprintf(", and helm deletes %s with the release", strings.Join(byHelm, ", "))
			choices = []Choice{choices[0], choices[2]}
		}
		a, err := o.Prompt.Ask(ctx, Form{
			Questions: []Question{{Title: title, Key: "confirm", Choices: choices}},
			Defaults:  func(map[string]string) map[string]string { return map[string]string{"confirm": "no"} },
		})
		if err != nil {
			return err
		}
		switch a["confirm"] {
		case "keep":
		case "data":
			data = true
		default:
			return ErrAborted
		}
		asked = true
	}
	// helm deletes the claims the chart makes with the release: their data goes only when the
	// operator says so, never as the side effect of an uninstall that keeps the rest.
	if !data && len(byHelm) > 0 {
		return fmt.Errorf("the chart of release %s makes the PersistentVolumeClaims %s, which `helm uninstall` deletes with it: `%s --data` uninstalls it with its data (back it up first)",
			k.Release, strings.Join(byHelm, ", "), in.k8sCtl("uninstall"))
	}
	// Kept data keeps the Secrets that answer for it: the values it was made with (a database's
	// password) and what the provision hook returned; an install of the release reads them back.
	// Data stays when its claims are not deleted, when the chart keeps some, and when the release
	// is gone (its claims cannot be told, so none is deleted, --data or not).
	var keptSecrets []string
	if (len(claims) > 0 && !data) || len(keptClaims) > 0 || rel == nil {
		owned = slices.DeleteFunc(owned, func(ob k8sOwned) bool {
			keep := ob.kind == "secret" && (ob.name == k.envSecret() || ob.name == k.hookName())
			if keep {
				keptSecrets = append(keptSecrets, ob.name)
			}
			return keep
		})
	}
	if rel == nil && len(owned) == 0 {
		return fmt.Errorf("no release %s in %s, and nothing %s made for it there but the Secrets %s, which answer for its data: once the data's claims are deleted, `kubectl delete secret %s --namespace %s` deletes them",
			k.Release, k.Namespace, in.R.Ctl(), strings.Join(keptSecrets, ", "), strings.Join(keptSecrets, " "), k.Namespace)
	}
	if len(owned) > 0 {
		var names []string
		for _, ob := range owned {
			names = append(names, ob.kind+" "+ob.name)
		}
		what = append(what, "delete what the installer made beside it ("+strings.Join(names, ", ")+")")
	}
	if data {
		what = append(what, "delete its PersistentVolumeClaims "+strings.Join(claims, ", "))
	}
	summary := strings.Join(what, ", ")
	if o.DryRun {
		fmt.Fprintf(o.Out, "a run would %s\n", summary)
		if len(claims) > 0 && !data {
			fmt.Fprintf(o.Out, "it would keep the PersistentVolumeClaims %s (--data deletes them)\n", strings.Join(claims, ", "))
		}
		if len(keptSecrets) > 0 {
			fmt.Fprintf(o.Out, "it would keep the Secrets %s, which answer for the data kept\n", strings.Join(keptSecrets, ", "))
		}
		return nil
	}
	if !o.Yes && !asked {
		if o.Prompt == nil {
			return errors.New("uninstall asks before it removes the release, and there is no terminal to answer: give --yes")
		}
		a, err := o.Prompt.Ask(ctx, Form{
			Questions: []Question{{Title: "Uninstall " + name + ": " + summary, Key: "confirm",
				Choices: []Choice{{"no", "No, change nothing"}, {"yes", "Yes, uninstall it"}}}},
			Defaults: func(map[string]string) map[string]string { return map[string]string{"confirm": "no"} },
		})
		if err != nil {
			return err
		}
		if a["confirm"] != "yes" {
			return ErrAborted
		}
	}
	timeout := k.Timeout.String()
	if rel != nil {
		o.logf("helm: uninstalling %s in %s", k.Release, k.Namespace)
		if _, err := in.helm(ctx, Cmd{Args: []string{"uninstall", k.Release, "--wait", "--timeout", timeout}, Stream: o.Log}); err != nil {
			return err
		}
	}
	for _, ob := range owned {
		args := []string{"delete", ob.kind, ob.name, "--ignore-not-found"}
		if ob.kind == "job" {
			args = append(args, "--cascade=foreground", "--wait", "--timeout", timeout)
		}
		if _, err := in.kubectl(ctx, Cmd{Args: args}); err != nil {
			return err
		}
	}
	if data {
		o.logf("kubectl: deleting the PersistentVolumeClaims %s", strings.Join(claims, ", "))
		args := append([]string{"delete", "persistentvolumeclaims"}, claims...)
		if _, err := in.kubectl(ctx, Cmd{Args: append(args, "--ignore-not-found", "--wait", "--timeout", timeout), Stream: o.Log}); err != nil {
			return err
		}
	}
	if rel != nil {
		fmt.Fprintf(o.Out, "release %s in %s is uninstalled\n", k.Release, k.Namespace)
	} else {
		fmt.Fprintf(o.Out, "release %s in %s was uninstalled already: what the installer left of it is deleted\n", k.Release, k.Namespace)
	}
	var kept []string
	if !data && len(claims) > 0 {
		// The release is gone now: a later uninstall cannot tell them, so the command is kubectl's.
		kept = append(kept, fmt.Sprintf("its PersistentVolumeClaims %s, which hold its data (`kubectl delete persistentvolumeclaims %s --namespace %s` deletes them)",
			strings.Join(claims, ", "), strings.Join(claims, " "), k.Namespace))
	}
	if len(keptClaims) > 0 {
		kept = append(kept, fmt.Sprintf("the PersistentVolumeClaims %s, which its chart keeps (`kubectl delete persistentvolumeclaims %s --namespace %s` deletes them)",
			strings.Join(keptClaims, ", "), strings.Join(keptClaims, " "), k.Namespace))
	}
	if rel == nil {
		kept = append(kept, "any PersistentVolumeClaim the release made: helm no longer knows it, so they cannot be told (`kubectl get persistentvolumeclaims --namespace "+k.Namespace+"` lists them)")
	}
	if len(keptSecrets) > 0 {
		then := fmt.Sprintf("once the data's claims are deleted, `kubectl delete secret %s --namespace %s` deletes them", strings.Join(keptSecrets, " "), k.Namespace)
		kept = append(kept, fmt.Sprintf("the Secrets %s, which answer for the data kept: `%s` takes it back with them (%s)", strings.Join(keptSecrets, ", "), in.k8sCtl("install"), then))
	}
	if len(foreign) > 0 {
		kept = append(kept, fmt.Sprintf("%s, which %s did not make for this release", strings.Join(foreign, ", "), in.R.Ctl()))
	}
	kept = append(kept, "the namespace "+k.Namespace)
	fmt.Fprintf(o.Out, "kept: %s\n", strings.Join(kept, "; "))
	return nil
}
