package tune

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// NICSettings are an interface's settings in ethtool's own terms (host.interfaces.<role>.ethtool):
// its combined channels and its rings (max, or a count; empty leaves them), its features by name
// (on or off) and its Energy-Efficient Ethernet (on, off or empty).
type NICSettings struct {
	Combined, RX, TX string
	Features         map[string]string
	EEE              string
}

// commands are the ethtool commands that give dev the settings, for an operator of a host Stowage
// does not own.
func (n NICSettings) commands(dev string) []string {
	var out []string
	if n.Combined != "" {
		out = append(out, "ethtool -L "+dev+" combined "+n.Combined)
	}
	var g []string
	for _, r := range [][2]string{{"rx", n.RX}, {"tx", n.TX}} {
		if r[1] != "" {
			g = append(g, r[0]+" "+r[1])
		}
	}
	if len(g) > 0 {
		out = append(out, "ethtool -G "+dev+" "+strings.Join(g, " "))
	}
	for _, f := range slices.Sorted(maps.Keys(n.Features)) {
		out = append(out, "ethtool -K "+dev+" "+f+" "+n.Features[f])
	}
	if n.EEE != "" {
		out = append(out, "ethtool --set-eee "+dev+" eee "+n.EEE)
	}
	return out
}

// channels are a NIC's combined channels, its maximum and now (ethtool -l); 0 for none listed.
type channels struct{ max, now uint64 }

func (s System) ethtoolChannels(ctx context.Context, eth, dev string) (channels, error) {
	out, err := s.Run(ctx, eth, "-l", dev)
	if err != nil {
		return channels{}, err
	}
	var c channels
	var at *uint64
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Pre-set maximums"):
			at = &c.max
		case strings.HasPrefix(line, "Current hardware settings"):
			at = &c.now
		}
		if k, v, ok := strings.Cut(line, ":"); ok && k == "Combined" && at != nil {
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
				*at = n
			}
		}
	}
	return c, nil
}

// wantCount is a setting of max or a count against the device's maximum: the count it asks, never
// past the maximum; 0 when it asks none.
func wantCount(v string, most uint64) uint64 {
	if v == "max" {
		return most
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0
	}
	if most > 0 {
		return min(n, most)
	}
	return n
}

// setFeatures sets each of want's features on dev on its own, where the driver lets it be changed:
// one fixed feature in one ethtool -K refuses them all (ENA's), so each is a command of its own,
// and a fixed one is left, noted.
func (s System) setFeatures(ctx context.Context, eth, dev string, want map[string]string, noteFixed bool) []string {
	have, err := s.ethtoolFeatures(ctx, eth, dev)
	if err != nil {
		return []string{fmt.Sprintf("ethtool -k %s: %v", dev, err)}
	}
	var notes []string
	for _, f := range slices.Sorted(maps.Keys(want)) {
		cur, ok := have[f]
		switch {
		case !ok || strings.Contains(cur, "[fixed]"):
			if noteFixed {
				notes = append(notes, fmt.Sprintf("%s %s: %s, the driver's: left", dev, f, cmp.Or(cur, "absent")))
			}
		case strings.HasPrefix(cur, want[f]):
		default:
			if _, err := s.Run(ctx, eth, "-K", dev, f, want[f]); err != nil {
				notes = append(notes, fmt.Sprintf("ethtool -K %s %s %s: %v", dev, f, want[f], err))
			}
		}
	}
	return notes
}

// applyNIC gives an interface of the project its ethtool settings: its features, its channels
// (an E810 whose RDMA holds the port answers EBUSY: said, with what frees it), its rings (on a
// link not yet up: a resize resets it) and its EEE.
func (s System) applyNIC(ctx context.Context, eth string, i Interface) []string {
	n, dev := i.NIC, i.Role
	if n == nil {
		return nil
	}
	notes := s.setFeatures(ctx, eth, dev, n.Features, true)
	if n.Combined != "" {
		if c, err := s.ethtoolChannels(ctx, eth, dev); err != nil {
			notes = append(notes, fmt.Sprintf("ethtool -l %s: %v", dev, err))
		} else if w := wantCount(n.Combined, c.max); w > 0 && w != c.now {
			if _, err := s.Run(ctx, eth, "-L", dev, "combined", nums(w)); err != nil {
				notes = append(notes, s.channelsRefused(dev, w, err))
			}
		}
	}
	if (n.RX != "" || n.TX != "") && !linkUp(s, dev) {
		if r, err := s.ethtoolRings(ctx, eth, dev); err == nil {
			var args []string
			if w := wantCount(n.RX, r.maxRX); w > 0 && w != r.rx {
				args = append(args, "rx", nums(w))
			}
			if w := wantCount(n.TX, r.maxTX); w > 0 && w != r.tx {
				args = append(args, "tx", nums(w))
			}
			if len(args) > 0 {
				if _, err := s.Run(ctx, eth, append([]string{"-G", dev}, args...)...); err != nil {
					notes = append(notes, fmt.Sprintf("ethtool -G %s: %v", dev, err))
				}
			}
		}
	}
	if n.EEE != "" {
		if _, err := s.Run(ctx, eth, "--set-eee", dev, "eee", n.EEE); err != nil {
			notes = append(notes, fmt.Sprintf("ethtool --set-eee %s eee %s: %v", dev, n.EEE, err))
		}
	}
	return notes
}

// channelsRefused says why ethtool -L was refused, where the cause is known: Intel's E810 (ice)
// answers EBUSY while its RDMA driver (irdma) holds the port's auxiliary device.
func (s System) channelsRefused(dev string, want uint64, err error) string {
	driver, _ := os.Readlink(s.path("sys/class/net/" + dev + "/device/driver"))
	if filepath.Base(driver) == "ice" && s.exists("sys/module/irdma") {
		return fmt.Sprintf("ethtool -L %s combined %d: %v: the E810's RDMA driver holds the port's queues (irdma); host.modules { irdma: { block: true } } frees them, its RDMA with them", dev, want, err)
	}
	return fmt.Sprintf("ethtool -L %s combined %d: %v", dev, want, err)
}

// inspectNIC judges an interface's ethtool settings on a host Stowage owns: each feature (a fixed
// one n/a), its channels and its rings against what they ask, within the device's maximum.
func (s System) inspectNIC(ctx context.Context, eth string, i Interface) []Item {
	n, dev := i.NIC, i.Role
	if n == nil {
		return nil
	}
	var items []Item
	if len(n.Features) > 0 {
		have, err := s.ethtoolFeatures(ctx, eth, dev)
		for _, f := range slices.Sorted(maps.Keys(n.Features)) {
			cur, ok := have[f]
			switch {
			case err != nil:
				items = append(items, Item{NA, "interface", dev + " " + f, "ethtool -k: " + err.Error(), n.Features[f]})
			case !ok || strings.Contains(cur, "[fixed]"):
				items = append(items, Item{NA, "interface", dev + " " + f, cmp.Or(cur, "absent") + ": the driver decides it", n.Features[f]})
			default:
				items = append(items, Item{verdict(strings.HasPrefix(cur, n.Features[f])), "interface", dev + " " + f, cur, n.Features[f]})
			}
		}
	}
	if n.Combined != "" {
		if c, err := s.ethtoolChannels(ctx, eth, dev); err == nil {
			w := wantCount(n.Combined, c.max)
			items = append(items, Item{verdict(w == 0 || c.now == w), "interface", dev + " channels", "combined " + nums(c.now), "combined " + nums(w)})
		}
	}
	if n.RX != "" || n.TX != "" {
		if r, err := s.ethtoolRings(ctx, eth, dev); err == nil {
			wr, wt := wantCount(n.RX, r.maxRX), wantCount(n.TX, r.maxTX)
			ok := (wr == 0 || r.rx == wr) && (wt == 0 || r.tx == wt)
			items = append(items, Item{verdict(ok), "interface", dev + " rings", fmt.Sprintf("rx %d tx %d", r.rx, r.tx), fmt.Sprintf("rx %d tx %d", cmp.Or(wr, r.rx), cmp.Or(wt, r.tx))})
		}
	}
	return items
}

// declaresRings reports whether the project gives the NIC dev its rings itself
// (host.interfaces.<dev>.ethtool.rings): the profile's sizing leaves it.
func (s System) declaresRings(dev string) bool {
	for _, i := range s.Interfaces {
		if i.Role == dev && i.NIC != nil && (i.NIC.RX != "" || i.NIC.TX != "") {
			return true
		}
	}
	return false
}
