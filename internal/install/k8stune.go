package install

import (
	"fmt"
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
	return nil
}
