package install

import (
	"slices"

	"github.com/norvexlabs/stowage/internal/providers"
)

// removed is what an upgrade from the release from to the release to drops: the inputs from
// declares that to no longer records (as an input on any shape, or a secret), and the keys from's provision hook declared it
// returns that to's no longer declares. A recorded value of either is dropped: the stack, the
// chart and the hook never receive it again, and nothing asks for it. A rollback takes it back from
// the release it returns to.
type removed struct {
	inputs, returns []string
}

func removedBy(from, to *Release) removed {
	var r removed
	// A key the release now records as a secret (and its rotation's) keeps the value it has. One
	// now a safe default is dropped: the product's safe value replaces the operator's.
	still := map[string]bool{}
	for _, s := range to.Spec.Secrets {
		still[s.Key], still[s.Key+"_NEXT"], still[s.Key+"_PREVIOUS"], still[s.Key+"_ACCEPTED"] = true, true, true, true
	}
	for _, i := range from.Spec.Inputs {
		if _, ok := to.Spec.Input(i.Key); !ok && !still[i.Key] && !slices.Contains(r.inputs, i.Key) {
			r.inputs = append(r.inputs, i.Key)
		}
	}
	returns := func(rel *Release) []string {
		h, err := providers.DecodeProvision(rel.Spec.Short(), rel.Spec.Hooks.Provision, rel.Spec.Images)
		if err != nil || h == nil {
			return nil
		}
		return h.Returns
	}
	now := returns(to)
	for _, k := range returns(from) {
		if !slices.Contains(now, k) {
			r.returns = append(r.returns, k)
		}
	}
	slices.Sort(r.inputs)
	slices.Sort(r.returns)
	return r
}
