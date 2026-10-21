package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/norvexlabs/stowage/internal/spec"
)

// The disk-pressure watch (hooks.maintenance.disk_pressure): as it starts, then every
// pressureEvery, the daemon reads the persistent partition's use; at soft_pct or past it, it POSTs
// the level to the hook's service, at its address on the stack's network. Past hard_pct, the
// console's login screen says so.

// pressureEvery and pressureRetry, the wait after a check that may succeed sooner, are counted in
// Hostd.Unit.
const (
	pressureEvery   = 600
	pressureRetry   = 60
	pressureTimeout = 30 * time.Second
	// maxPressureAnswer bounds the part of an answer the daemon reads, to log it.
	maxPressureAnswer = 512
)

// DiskPressure is the disk-pressure watch's reading in GET /v1/status.
type DiskPressure struct {
	Level   string  `json:"level"` // none, soft or hard; unknown when the disk cannot be read
	UsedPct float64 `json:"used_pct"`
	SoftPct int     `json:"soft_pct"`
	HardPct int     `json:"hard_pct"`
	// Last is the watch's last POST; nil before the first.
	Last *PressurePost `json:"last_post,omitempty"`
}

// PressurePost is one POST of the watch.
type PressurePost struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	UsedPct float64   `json:"used_pct"`
	// Result: the service's status code, or why the POST was not made or not answered.
	Result string `json:"result"`
	OK     bool   `json:"ok"` // the service answered 2xx
}

// PressureBody is the body the watch POSTs.
type PressureBody struct {
	Level   string  `json:"level"`
	UsedPct float64 `json:"used_pct"`
	SoftPct int     `json:"soft_pct"`
	HardPct int     `json:"hard_pct"`
}

// pressureHook is the spec's disk-pressure hook; nil when it has none.
func (h *Hostd) pressureHook() *spec.DiskPressure {
	if m := h.R.Spec.Hooks.Maintenance; m != nil {
		return m.DiskPressure
	}
	return nil
}

// diskPath is the directory whose filesystem the daemon reads: the install directory's parent, on
// the VM the persistent partition, which also holds Docker's volumes (Kairos bind-mounts /var/lib
// from it).
func (h *Hostd) diskPath() string {
	if h.DiskDir != "" {
		return h.DiskDir
	}
	dir, err := filepath.Abs(h.O.Dir)
	if err != nil {
		dir = h.O.Dir
	}
	return filepath.Dir(dir)
}

func (h *Hostd) disk() *Disk {
	if h.Statfs != nil {
		return h.Statfs(h.diskPath())
	}
	return diskOf(h.diskPath())
}

// pressureOf is d's use in percent as df counts it — used of used and available, the blocks
// reserved for root counted in neither —, rounded down to a tenth, and its level against dp: hard
// at hard_pct or past it, soft at soft_pct or past it, none below; empty when d cannot be read.
// The level is decided on the exact use, so a rounded 74.9 may not be soft yet.
func pressureOf(d *Disk, dp *spec.DiskPressure) (string, float64) {
	if d == nil || d.Used+d.Available == 0 {
		return "", 0
	}
	used := float64(d.Used) * 100 / float64(d.Used+d.Available)
	level := "none"
	switch {
	case used >= float64(dp.HardPct):
		level = "hard"
	case used >= float64(dp.SoftPct):
		level = "soft"
	}
	return level, math.Floor(used*10) / 10
}

// pressureStatus is the watch's reading for the status: the disk read now, the last POST.
func (h *Hostd) pressureStatus(d *Disk) *DiskPressure {
	dp := h.pressureHook()
	if dp == nil {
		return nil
	}
	st := &DiskPressure{SoftPct: dp.SoftPct, HardPct: dp.HardPct}
	st.Level, st.UsedPct = pressureOf(d, dp)
	if st.Level == "" {
		st.Level = "unknown"
	}
	h.pressMu.Lock()
	if h.lastPost != nil {
		p := *h.lastPost
		st.Last = &p
	}
	h.pressMu.Unlock()
	return st
}

// pressureWatch checks at once, then every pressureEvery, until ctx ends. Without the hook, it
// removes the banner a release that had one left, and returns.
func (h *Hostd) pressureWatch(ctx context.Context) {
	dp := h.pressureHook()
	if dp == nil {
		h.pressureBanner(ctx, "")
		return
	}
	for {
		wait := pressureEvery
		if h.pressureCheck(ctx, dp) {
			wait = pressureRetry
		}
		t := time.NewTimer(time.Duration(wait) * h.unit())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// pressureCheck reads the disk once: it writes or removes the banner, and at soft or hard POSTs
// the level. It reports whether to check again sooner than pressureEvery: the disk could not be
// read, or the POST may be taken in a minute (not sent as the stack is not up — at boot the daemon
// starts before it —, no answer, a 5xx).
func (h *Hostd) pressureCheck(ctx context.Context, dp *spec.DiskPressure) bool {
	d := h.disk()
	level, used := pressureOf(d, dp)
	if level == "" {
		h.O.logf("disk pressure: cannot read the disk of %s", h.diskPath())
		return true
	}
	banner := ""
	if level == "hard" {
		banner = fmt.Sprintf("disk: %.1f%% used on %s, past %d%%: free space or grow the disk (`%s status`)\n",
			used, d.Path, dp.HardPct, h.R.Ctl())
	}
	h.pressureBanner(ctx, banner)
	if level == "none" {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, pressureTimeout)
	defer cancel()
	result, ok, again := h.pressurePost(pctx, dp, PressureBody{Level: level, UsedPct: used, SoftPct: dp.SoftPct, HardPct: dp.HardPct})
	if ctx.Err() != nil && !ok {
		return false // the daemon stops: the check was cut off, not refused
	}
	h.O.logf("disk pressure %s, %.1f%% used: POST %s: %s", level, used, dp.Post, result)
	post := PressurePost{At: time.Now().UTC(), Level: level, UsedPct: used, Result: result, OK: ok}
	h.pressMu.Lock()
	h.lastPost = &post
	h.pressMu.Unlock()
	if h.Docker {
		h.savePressure(post)
	}
	return again
}

// pressurePost POSTs body to the hook's service, at its container's address with the name `post`
// gives as the Host, the auth secret as a bearer token when the hook names one; ctx bounds it
// all. It answers what came of it, whether the service took it (2xx), and whether it may be
// taken soon: it sends nothing before the first install, and nothing when the service has no
// address (the stack is down, or being upgraded).
func (h *Hostd) pressurePost(ctx context.Context, dp *spec.DiskPressure, body PressureBody) (result string, ok, again bool) {
	const key = "hooks.maintenance.disk_pressure"
	u, err := url.Parse(dp.Post)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.Port() == "" {
		return fmt.Sprintf("not sent: %s.post %q is not http://<service>:<port>/…", key, dp.Post), false, false
	}
	in, _ := h.run()
	if notInstalled(in) {
		return "not sent: not installed yet", false, true
	}
	env, err := in.open()
	if err != nil {
		return fmt.Sprintf("not sent: %v", err), false, true
	}
	token := ""
	// No result carries the token, whatever echoes it (an error's output, the service).
	defer func() {
		if token != "" {
			result = strings.ReplaceAll(result, token, "<"+dp.AuthSecret+">")
		}
	}()
	if dp.AuthSecret != "" {
		var found bool
		if token, found = env.Get(dp.AuthSecret); !found || token == "" {
			return fmt.Sprintf("not sent: the install has no %s (%s.auth_secret)", dp.AuthSecret, key), false, false
		}
	}
	ip, err := in.address(ctx, env, u.Hostname())
	if err != nil {
		return fmt.Sprintf("not sent: %v", err), false, true
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Sprintf("not sent: %v", err), false, false
	}
	target := *u
	target.Host = net.JoinHostPort(ip, u.Port())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(raw))
	if err != nil {
		return fmt.Sprintf("not sent: %v", err), false, false
	}
	req.Host = u.Host
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := cmp.Or(h.Probe, probes).Do(req)
	if err != nil {
		// The URL error names the address, never the token: it is in a header.
		return fmt.Sprintf("no answer: %v", err), false, true
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return fmt.Sprintf("answered %d", resp.StatusCode), true, false
	}
	again = resp.StatusCode >= 500
	if token != "" {
		// An answer may echo the token in any form (cut, encoded): only its status is kept.
		return fmt.Sprintf("answered %d", resp.StatusCode), false, again
	}
	ans, _ := io.ReadAll(io.LimitReader(resp.Body, maxPressureAnswer))
	msg := strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(string(ans)))
	if msg == "" {
		return fmt.Sprintf("answered %d", resp.StatusCode), false, again
	}
	return fmt.Sprintf("answered %d: %s", resp.StatusCode, msg), false, again
}

// pressureBanner writes text as the disk banner, or removes it when text is empty; the login
// screen is reloaded only when the banner changed.
func (h *Hostd) pressureBanner(ctx context.Context, text string) {
	if h.Docker {
		return // a Docker host's console is not the install's
	}
	p := filepath.Join(cmp.Or(h.Issue, "/etc/issue.d"), "08-"+h.R.Spec.Name+"-disk.issue")
	old, err := os.ReadFile(p)
	switch {
	case errors.Is(err, os.ErrNotExist) && text == "":
		return // none, and none wanted
	case err == nil && string(old) == text:
		return
	case text == "":
		if err := os.Remove(p); err != nil {
			h.O.logf("disk pressure: removing the banner: %v", err)
			return
		}
	default:
		err := os.MkdirAll(filepath.Dir(p), 0o755)
		if err == nil {
			err = writeAtomic(p, strings.NewReader(text), 0o644, nil)
		}
		if err != nil {
			h.O.logf("disk pressure: writing the banner: %v", err)
			return
		}
	}
	h.reloadBanner(ctx)
}
