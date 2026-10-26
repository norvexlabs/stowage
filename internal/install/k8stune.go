package install

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/norvexlabs/stowage/internal/tune"
)

// podSysctls are the chart's stowage.sysctls: tune.Pod as a pod's securityContext.sysctls.
func podSysctls() []map[string]string {
	var out []map[string]string
	for _, s := range tune.Pod() {
		out = append(out, map[string]string{"name": s.Key, "value": s.Want})
	}
	return out
}

// K8sTune prints what tunes the release's network on Kubernetes: the pods' settings
// --unsafe-sysctls gives, the kubelet setting that allows them, and the nodes' own tuning, which
// the node's `tune` does.
func (in *Installer) K8sTune() error {
	w := in.O.Out
	pod := tune.Pod()
	keys := make([]string, len(pod))
	fmt.Fprintf(w, "The pods' network settings, given by `%s --unsafe-sysctls` (and `upgrade`):\n", in.k8sCtl("install"))
	for i, s := range pod {
		keys[i] = s.Key
		fmt.Fprintf(w, "  %s = %s   %s\n", s.Key, s.Want, s.Why)
	}
	fmt.Fprintf(w, "\nThe kubelet runs a pod with them only when its configuration file (KubeletConfiguration) allows them,\non every node; a pod it does not allow fails (SysctlForbidden), and Pod Security admits them only at\nthe privileged level:\n\n  allowedUnsafeSysctls:\n")
	for _, k := range keys {
		fmt.Fprintf(w, "    - %s\n", k)
	}
	fmt.Fprintf(w, "\nor its flag --allowed-unsafe-sysctls=%s\n", strings.Join(keys, ","))
	fmt.Fprintf(w, "\nThe nodes' own settings (the TCP buffers a pod's namespace copies, the sockets' memory, the connection\ntracker, the NICs) are the node's: `%s tune` on a node reports them, `tune --apply` applies them,\n`tune --print` prints the files for a node configured by other means.\n", in.R.Ctl())
	var sys tune.System
	releaseHost(&sys, in.R.Spec)
	mods, pkgs := sys.Modules, sys.Packages
	if len(mods)+len(pkgs)+len(sys.KernelArgs)+len(sys.Settings) == 0 && sys.Power != tune.Performance {
		return nil
	}
	fmt.Fprintf(w, "\nWhat the project asks of every node (host), which the node's operator gives it:\n")
	for _, m := range mods {
		var want []string
		switch {
		case m.Load:
			want = append(want, "loaded at boot")
		case m.Block:
			want = append(want, "blocked")
		}
		for _, o := range slices.Sorted(maps.Keys(m.Options)) {
			want = append(want, o+"="+m.Options[o])
		}
		fmt.Fprintf(w, "  module %s: %s   %s\n", m.Name, strings.Join(want, ", "), m.Reason)
	}
	for _, p := range pkgs {
		fmt.Fprintf(w, "  package %s (Debian's name)   %s\n", p.Name, p.Reason)
	}
	for _, a := range sys.KernelArgs {
		fmt.Fprintf(w, "  kernel argument %s   %s\n", a, a.Reason)
	}
	for _, c := range sys.Settings {
		fmt.Fprintf(w, "  sysctl %s = %s   %s\n", c.Key, c.Want, c.Why)
	}
	if sys.Power == tune.Performance {
		fmt.Fprintf(w, "  power performance: every CPU on the performance governor, its energy preference performance, intel_pstate's min_perf_pct 100\n")
	}
	return nil
}
