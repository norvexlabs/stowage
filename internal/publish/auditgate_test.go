package publish

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/sbom"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// gateScanner is a scanner whose grype finds, in each SBOM naming the repository of one of
// vulns' keys (an image key, installer or os), that key's finding.
func gateScanner(t *testing.T, vulns map[string]sbom.Finding) *sbom.Scanner {
	run := func(_ context.Context, stdout io.Writer, args ...string) error {
		dir := ""
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "type=bind,src="); ok && strings.HasSuffix(v, ",dst=/in,readonly") {
				dir = strings.TrimSuffix(v, ",dst=/in,readonly")
			}
		}
		if dir == "" { // the database made ready
			return nil
		}
		doc, err := os.ReadFile(filepath.Join(dir, "sbom.json"))
		if err != nil {
			return err
		}
		type fix struct {
			Versions []string `json:"versions"`
		}
		type vuln struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Fix      fix    `json:"fix"`
		}
		type art struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Type    string `json:"type"`
		}
		type match struct {
			Vulnerability vuln `json:"vulnerability"`
			Artifact      art  `json:"artifact"`
		}
		matches := []match{}
		for key, f := range vulns {
			if strings.Contains(string(doc), "/notes-"+key+"@") {
				matches = append(matches, match{vuln{f.ID, f.Severity.String(), fix{f.Fixed}}, art{f.Package, f.Version, "apk"}})
			}
		}
		return json.NewEncoder(stdout).Encode(map[string]any{"matches": matches})
	}
	return &sbom.Scanner{Docker: run, Cache: t.TempDir()}
}

// gateEnv is a signing env whose spec declares audit (a YAML flow mapping) and whose scanner finds
// vulns.
func gateEnv(t *testing.T, audit string, vulns map[string]sbom.Finding) *env {
	e := newEnv(t)
	e.sign()
	testutil.Write(t, e.root, "stowage.yaml", readFile(t, e.root+"/stowage.yaml")+"audit: "+audit+"\n")
	e.scan = gateScanner(t, vulns)
	return e
}

var webHigh = map[string]sbom.Finding{"web": {ID: "CVE-W", Severity: sbom.High, Package: "libcurl", Version: "8.22.0", Fixed: []string{"8.23.0"}}}

// A finding the gate refuses stops the ship before the release manifest is written and before a
// channel points at it; an override publishes past it and is recorded on the release, and kept by
// a yank; an unchanged release is judged again.
func TestTheAuditGateRefusesBeforeTheChannelMoves(t *testing.T) {
	t.Parallel()
	e := gateEnv(t, "{ fail_on: high }", webHigh)
	_, err := e.ship("stable", 1)
	if err == nil || !strings.Contains(err.Error(), "audit: 1 fixable vulnerabilities at or above high: high CVE-W in web (libcurl 8.22.0, fixed in 8.23.0)") ||
		!strings.Contains(err.Error(), "no channel moves to it") {
		t.Fatalf("%v", err)
	}
	p, v := e.publisher()
	tag, _ := p.releaseTag(v, "stable", 1)
	if r, err := p.readRelease(context.Background(), tag); r != nil || err != nil {
		t.Fatalf("a release manifest written: %v %v", r, err)
	}
	if r, err := p.pointer(context.Background(), "stable"); r != nil || err != nil {
		t.Fatalf("the channel moved: %v %v", r, err)
	}
	// No release tag holds an artifact of the refused release: the installer's, which a bootstrap
	// runs, the images' and the bundle's.
	for _, repo := range []string{p.installerRepo(), p.imageRepo("web"), p.imageRepo("api"), p.osRepo()} {
		rt, _ := p.Registry.Tag(repo, tag)
		if have, err := p.Registry.Head(context.Background(), rt); have != nil || err != nil {
			t.Fatalf("%s moved: %v %v", rt, have, err)
		}
	}
	if !strings.Contains(e.log.String(), "CVE-W") || !strings.Contains(e.log.String(), "the project's: fixable 1 high") {
		t.Errorf("the refused findings are not printed:\n%s", e.log)
	}

	e.override = "the fix breaks the api; mitigated by the front"
	r := e.mustShip("stable", 1)
	eq(t, "the override on the ref", r.AuditOverride, e.override)
	p, _ = e.publisher()
	got, err := p.readRelease(context.Background(), tag)
	if err != nil || got.AuditOverride != e.override {
		t.Fatalf("the override recorded: %+v %v", got, err)
	}
	if ptr, _ := p.pointer(context.Background(), "stable"); ptr == nil || ptr.Digest != r.Digest {
		t.Fatal("the channel does not point at the release")
	}

	// Unchanged, it is judged again: without the override it is refused, and its message says the
	// ship moves nothing (stable points at it); with one, the override recorded on it stands.
	e.override = ""
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "CVE-W") || !strings.Contains(err.Error(), "this ship moves nothing (a channel already pointing at the release stays there)") {
		t.Fatalf("an unchanged release re-shipped past the gate: %v", err)
	}
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "ship or promote it again with --audit-override REASON to keep that override") {
		t.Fatalf("the hint for a recorded override: %v", err)
	}
	e.override = "another reason"
	if r := e.mustShip("stable", 1); r.AuditOverride != "the fix breaks the api; mitigated by the front" {
		t.Fatalf("the recorded override: %q", r.AuditOverride)
	}
	if !strings.Contains(e.log.String(), "by the override recorded on it: the fix breaks the api") {
		t.Errorf("the recorded override is not logged:\n%s", e.log)
	}
	e.override = ""
	p, v = e.publisher()
	if err := p.Yank(context.Background(), "stable", v, 1, "broken"); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.readRelease(context.Background(), tag); got == nil || got.Manifest.Yanked == nil || got.AuditOverride != "the fix breaks the api; mitigated by the front" {
		t.Fatalf("the yank lost the override: %+v", got)
	}
}

// An allowance in force passes the finding; expired (its last day the day before), the finding
// stops the ship again and the publisher is told.
func TestTheAuditGateAllowance(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		expires string
		pass    bool
	}{{"2026-10-08", true}, {"2026-10-07", false}} { // the env's now is 2026-10-08 12:00 UTC
		e := gateEnv(t, "{ fail_on: high, allow: audit-allow.yaml }", webHigh)
		testutil.Write(t, e.root, "audit-allow.yaml", "- { id: CVE-W, artifact: web, reason: no fix in the base yet, expires: "+c.expires+" }\n")
		_, err := e.ship("stable", 1)
		if c.pass != (err == nil) {
			t.Fatalf("expires %s: %v", c.expires, err)
		}
		if !c.pass && !strings.Contains(e.log.String(), "allowance CVE-W in web expired on 2026-10-07") {
			t.Errorf("no note of the expired allowance:\n%s", e.log)
		}
		if c.pass && !strings.Contains(e.log.String(), "audit: allowance CVE-W in web until 2026-10-08 accepts 1 finding(s): no fix in the base yet") {
			t.Errorf("the allowance applied is not listed:\n%s", e.log)
		}
		if !c.pass && strings.Contains(e.log.String(), "accepts") {
			t.Errorf("an expired allowance is listed as applied:\n%s", e.log)
		}
		if c.pass && !strings.Contains(e.log.String(), "passes (") {
			t.Errorf("no pass logged:\n%s", e.log)
		}
	}
}

// A finding no version fixes passes by default, and stops the ship with fixable_only false; a
// finding below fail_on passes.
func TestTheAuditGateCountsWhatThePolicySays(t *testing.T) {
	t.Parallel()
	noFix := map[string]sbom.Finding{"api": {ID: "CVE-N", Severity: sbom.Critical, Package: "kernel", Version: "6"}}
	if _, err := gateEnv(t, "{ fail_on: high }", noFix).ship("stable", 1); err != nil {
		t.Errorf("a finding with no fix stopped the ship: %v", err)
	}
	if _, err := gateEnv(t, "{ fail_on: high, fixable_only: false }", noFix).ship("stable", 1); err == nil || !strings.Contains(err.Error(), "1 vulnerabilities at or above high: critical CVE-N in api") {
		t.Errorf("fixable_only false: %v", err)
	}
	if _, err := gateEnv(t, "{ fail_on: critical }", webHigh).ship("stable", 1); err != nil {
		t.Errorf("a finding below fail_on stopped the ship: %v", err)
	}
}

// An artifact whose scan failed is not audited: refused, whatever the override.
func TestTheAuditGateRefusesWhatItCannotScan(t *testing.T) {
	t.Parallel()
	e := gateEnv(t, "{ fail_on: critical }", nil)
	e.override = "anything"
	inner := e.scan.Docker
	e.scan.Docker = func(ctx context.Context, stdout io.Writer, args ...string) error {
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "type=bind,src="); ok && strings.HasSuffix(v, ",dst=/in,readonly") {
				doc, _ := os.ReadFile(filepath.Join(strings.TrimSuffix(v, ",dst=/in,readonly"), "sbom.json"))
				if strings.Contains(string(doc), "/notes-web@") {
					return errors.New("grype crashed")
				}
			}
		}
		return inner(ctx, stdout, args...)
	}
	_, err := e.ship("stable", 1)
	if err == nil || !strings.Contains(err.Error(), "audit: not audited: web") || !strings.Contains(err.Error(), "no override publishes past it") {
		t.Fatalf("%v", err)
	}
}

// A release published past no gate, shipped again unchanged after a finding is published, cannot
// take an override it would not record: a new revision does.
func TestTheAuditGateOverrideOnAnUnchangedRelease(t *testing.T) {
	t.Parallel()
	e := gateEnv(t, "{ fail_on: high }", nil)
	e.mustShip("stable", 1)
	e.scan = gateScanner(t, webHigh)
	e.override = "accepted"
	_, err := e.ship("stable", 1)
	if err == nil || !strings.Contains(err.Error(), "published unchanged without an override") || !strings.Contains(err.Error(), "publish a new revision of the release (2)") {
		t.Fatalf("%v", err)
	}
	r := e.mustShip("stable", 2)
	eq(t, "the new revision's override", r.AuditOverride, "accepted")
}

// A promotion passes the gate too: the release it publishes on the other channel is judged.
func TestTheAuditGateOnPromote(t *testing.T) {
	t.Parallel()
	e := gateEnv(t, "{ fail_on: high }", nil)
	e.mustShip("beta", 1)
	e.scan = gateScanner(t, webHigh)
	p, v := e.publisher()
	if _, err := p.Promote(context.Background(), v, "beta", "stable", 1); err == nil || !strings.Contains(err.Error(), "CVE-W") {
		t.Fatalf("a promotion past the gate: %v", err)
	}
	if r, _ := p.pointer(context.Background(), "stable"); r != nil {
		t.Fatal("stable moved")
	}
}

// What the gate cannot judge stops the ship before anything is published.
func TestTheAuditGateRefusals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, audit, allow, want string
		db, override             string
		noScan, unsigned         bool
	}{
		{name: "no scanner", audit: "{ fail_on: high }", noScan: true, want: "audit: no scanner is set"},
		{name: "fail_on", audit: "{ fail_on: none }", want: `audit.fail_on "none"`},
		{name: "allowlist", audit: "{ fail_on: high, allow: audit-allow.yaml }", allow: "- { id: CVE-W }\n", want: "audit.allow: audit-allow.yaml: entry 1 (CVE-W): artifact is required"},
		{name: "allowlist missing", audit: "{ fail_on: high, allow: audit-allow.yaml }", want: "audit.allow: open"},
		{name: "unsigned", audit: "{ fail_on: high }", unsigned: true, want: "audit: the project signs no release"},
		{name: "database", audit: "{ fail_on: high }", db: "no-such.tar.zst", want: "audit: the database: stat no-such.tar.zst"},
		{name: "override without a gate", override: "x", want: "--audit-override: the project declares no audit gate"},
		{name: "blank override", override: " \n", want: "--audit-override: the reason is blank"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var e *env
			if c.audit == "" {
				e = newEnv(t)
				e.sign()
			} else {
				e = gateEnv(t, c.audit, nil)
				e.scan.DB = c.db
			}
			e.override = c.override
			if c.noScan {
				e.scan = nil
			}
			if c.allow != "" {
				testutil.Write(t, e.root, "audit-allow.yaml", c.allow)
			}
			if c.unsigned {
				testutil.Edit(t, e.root, "stowage.yaml", "signing: { provider: cosign-key, key: env://SIGNING_KEY, public_keys: [keys/release.pub] }\n", "")
				e.signer = nil
			}
			_, err := e.ship("stable", 1)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
			if len(e.builder.built) > 0 || len(e.disks.made) > 0 {
				t.Errorf("built before the refusal: %v", e.builder.names())
			}
			p, _ := e.publisher()
			if r, _ := p.pointer(context.Background(), "stable"); r != nil {
				t.Fatal("stable moved")
			}
		})
	}
}

// A refused release registers no cloud image; once the gate passes, it does.
func TestTheAuditGateRegistersNoCloud(t *testing.T) {
	t.Parallel()
	e := gateEnv(t, "{ fail_on: high }", webHigh)
	b := readFile(t, e.root+"/stowage.yaml")
	at := strings.Index(b, "disk_formats:")
	line := b[strings.LastIndexByte(b[:at], '\n')+1 : at+len("disk_formats:")]
	testutil.Edit(t, e.root, "stowage.yaml", line, strings.TrimSuffix(line, "disk_formats:")+"cloud_publish: [gcp]\n"+line)
	e.cloud = &fakeCloud{dir: t.TempDir()}
	e.cloudVars = map[string]map[string]string{"gcp": {"project": "p", "bucket": "b"}}
	if _, err := e.ship("beta", 1); err == nil || !strings.Contains(err.Error(), "CVE-W") {
		t.Fatalf("%v", err)
	}
	if len(e.cloud.uploads) != 0 {
		t.Errorf("a refused release registered %d cloud images", len(e.cloud.uploads))
	}
	e.override = "accepted"
	e.mustShip("beta", 1)
	if len(e.cloud.uploads) != 1 {
		t.Errorf("%d cloud images registered once the gate passed", len(e.cloud.uploads))
	}
}

// A yanked release shipped again is refused as yanked, whatever the gate would say.
func TestTheAuditGateLeavesAYankedReleaseToItsRefusal(t *testing.T) {
	t.Parallel()
	e := gateEnv(t, "{ fail_on: high }", nil)
	e.mustShip("stable", 1)
	p, v := e.publisher()
	if err := p.Yank(context.Background(), "stable", v, 1, "broken"); err != nil {
		t.Fatal(err)
	}
	e.scan = gateScanner(t, webHigh)
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "is yanked") || strings.Contains(err.Error(), "audit") {
		t.Fatalf("%v", err)
	}
}

// A disk recorded by a release but gone from the registry (a newer set replaced it) does not stop
// the gate: the bundle's scan judges what the disk carries.
func TestTheAuditGateReadsNoDisk(t *testing.T) {
	t.Parallel()
	e := gateEnv(t, "{ fail_on: high }", nil)
	r := e.mustShip("stable", 1)
	p, _ := e.publisher()
	policy, err := p.auditPolicy()
	if err != nil {
		t.Fatal(err)
	}
	m := *r.Manifest
	m.Artifacts.Disks = map[string]string{"qcow2": "sha256:" + strings.Repeat("d", 64)}
	if _, err := p.auditGate(context.Background(), &m, policy, nil); err != nil {
		t.Fatalf("a gone disk: %v", err)
	}
}

// The reason is recorded less the blanks around it.
func TestTheAuditGateTrimsTheReason(t *testing.T) {
	t.Parallel()
	e := gateEnv(t, "{ fail_on: high }", webHigh)
	e.override = "  accepted for now\n"
	eq(t, "the recorded reason", e.mustShip("stable", 1).AuditOverride, "accepted for now")
}
