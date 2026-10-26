package install

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The module contract: the variables `vm create` writes into terraform.tfvars.json. A module
// declares every one of contractVars; it may declare any of optionalVars, written when it does:
// the VM's CPUs and memory (a cloud's module sizes it by its own instance type instead, and
// --cpus / --memory are refused), the image's disk format (one of the platform's), whether its
// IPv4 address comes by DHCP, and the administrator and their SSH key.
// Its outputs are `id` (required: what the platform calls the VM) and `address` (optional: where it
// answers, when the platform tells; empty otherwise).
var (
	contractVars = []string{"name", "description", "disk_gib", "image", "image_sha256", "seed", "seed_sha256"}
	optionalVars = []string{"cpus", "memory_mib", "image_format", "dhcp", "admin_user", "ssh_public_key", "nics"}
	// truenasVars are the TrueNAS module's own variables, which its flags give.
	truenasVars = []string{"insecure_skip_verify", "pool", "parent_dataset", "nic_attach"}
)

// platformFile names, in a VM's state directory, the platform it was created on and whether with
// the built-in module ("<key> builtin") or the operator's ("<key> module").
const platformFile = "platform"

// moduleVar is a variable a module declares.
type moduleVar struct {
	Name, Description string
	HasDefault        bool
}

var (
	hclVariable    = regexp.MustCompile(`^variable\s+"([A-Za-z_][A-Za-z0-9_-]*)"\s*\{\s*$`)
	hclOneLine     = regexp.MustCompile(`^variable\s+"([A-Za-z_][A-Za-z0-9_-]*)"\s*\{(.*)\}\s*$`)
	hclOneDefault  = regexp.MustCompile(`(^|\s)default\s*=`)
	hclString      = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	hclDescription = regexp.MustCompile(`^\s+description\s*=\s*"((?:[^"\\]|\\.)*)"\s*$`)
	hclDefault     = regexp.MustCompile(`^  default\s*=`) // a block's own attribute, as `tofu fmt` indents it
	hclNested      = regexp.MustCompile(`\([^()]*\)|\{[^{}]*\}`)
)

// moduleVars are the variables the module's files declare, sorted by name: its *.tf files read as
// `tofu fmt` writes them (a variable block opens at the start of a line and closes at a lone "}",
// or is on one line), and its *.tf.json files as JSON.
func moduleVars(files map[string][]byte) ([]moduleVar, error) {
	seen := map[string]moduleVar{}
	for _, name := range sortedKeys(files) {
		b := files[name]
		switch {
		case strings.HasSuffix(name, ".tf.json"):
			var doc struct {
				Variable map[string]struct {
					Description string `json:"description"`
					Default     any    `json:"default"`
				} `json:"variable"`
			}
			if err := json.Unmarshal(b, &doc); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			for n, v := range doc.Variable {
				seen[n] = moduleVar{n, v.Description, v.Default != nil}
			}
		case strings.HasSuffix(name, ".tf"):
			var cur *moduleVar
			sc := bufio.NewScanner(bytes.NewReader(b))
			for sc.Scan() {
				line := hclCode(sc.Text())
				if one := hclOneLine.FindStringSubmatch(line); cur == nil && one != nil {
					// The block's own attributes: its strings and nested types blanked.
					attrs := hclString.ReplaceAllString(one[2], `""`)
					for n := ""; n != attrs; {
						n, attrs = attrs, hclNested.ReplaceAllString(attrs, "")
					}
					seen[one[1]] = moduleVar{Name: one[1], HasDefault: hclOneDefault.MatchString(attrs)}
					continue
				}
				switch m := hclVariable.FindStringSubmatch(line); {
				case m != nil:
					cur = &moduleVar{Name: m[1]}
				case cur == nil:
				case line == "}":
					seen[cur.Name] = *cur
					cur = nil
				case hclDefault.MatchString(line):
					cur.HasDefault = true
				default:
					if d := hclDescription.FindStringSubmatch(line); d != nil {
						cur.Description = strings.ReplaceAll(d[1], `\"`, `"`)
					}
				}
			}
			if err := sc.Err(); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if cur != nil {
				return nil, fmt.Errorf("%s: variable %q is not closed by a lone \"}\" (`tofu fmt` writes it so)", name, cur.Name)
			}
		}
	}
	var out []moduleVar
	for _, n := range sortedKeys(seen) {
		out = append(out, seen[n])
	}
	return out, nil
}

// hclCode is a line of HCL less its line ending and a trailing comment (# or //) outside a string.
func hclCode(line string) string {
	line = strings.TrimRight(line, "\r")
	in := false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case in && c == '\\':
			i++
		case c == '"':
			in = !in
		case !in && (c == '#' || c == '/' && i+1 < len(line) && line[i+1] == '/'):
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return line
}

// moduleFiles are the files of a module: the built-in one of platform when dir is empty, else the
// files of dir OpenTofu reads as the root module (a symbolic link followed, as OpenTofu does; not
// its subdirectories), and its lock.
func moduleFiles(platform, dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if dir == "" {
		root := path.Join("tofu", platform)
		entries, err := fs.ReadDir(tofuModules, root)
		if err != nil {
			return nil, fmt.Errorf("no built-in OpenTofu module for %s", platform)
		}
		for _, e := range entries {
			b, err := fs.ReadFile(tofuModules, path.Join(root, e.Name()))
			if err != nil {
				return nil, err
			}
			files[e.Name()] = b
		}
		return files, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("--module: %w", err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !(strings.HasSuffix(n, ".tf") || strings.HasSuffix(n, ".tf.json") || n == ".terraform.lock.hcl") || n == varsFile {
			continue
		}
		if files[n], err = os.ReadFile(filepath.Join(dir, n)); err != nil {
			return nil, fmt.Errorf("--module: %w", err)
		}
	}
	if !slices.ContainsFunc(sortedKeys(files), func(n string) bool { return n != ".terraform.lock.hcl" }) {
		return nil, fmt.Errorf("--module %s: no *.tf or *.tf.json file", dir)
	}
	return files, nil
}

// checkContract refuses a module that does not declare a variable of the contract, and a variable
// given that it does not declare (a typo is never ignored).
func checkContract(vars []moduleVar, given map[string]string) error {
	declared := map[string]bool{}
	for _, v := range vars {
		declared[v.Name] = true
	}
	var missing []string
	for _, n := range contractVars {
		if !declared[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the module does not declare %s: a module `vm create` applies declares %s", strings.Join(missing, ", "), strings.Join(contractVars, ", "))
	}
	for _, k := range sortedKeys(given) {
		if !declared[k] {
			return fmt.Errorf("--tofu-var %s: the module declares no variable %s", k, k)
		}
	}
	return nil
}

// builtinTrueNAS reports whether the VM is made with TrueNAS's built-in module: its place comes
// from --pool, --dataset and --bridge, and its API's address and key from the environment.
func (t TofuVM) builtinTrueNAS(p Platform) bool { return builtinTrueNAS(p.Key, t.Module == "") }

// builtinTrueNAS reports whether a module of platform key, built-in or not, is TrueNAS's
// built-in one.
func builtinTrueNAS(key string, builtin bool) bool { return key == "truenas" && builtin }

// declares reports whether the module declares the variable name.
func declares(vars []moduleVar, name string) bool {
	return slices.ContainsFunc(vars, func(v moduleVar) bool { return v.Name == name })
}

// platformVars are the variables of a module the operator gives (--tofu-var, or asked): neither the
// contract's, which `vm create` writes, nor those the platform's own flags give.
func platformVars(vars []moduleVar, builtinTrueNAS bool) []moduleVar {
	return slices.DeleteFunc(slices.Clone(vars), func(v moduleVar) bool {
		return slices.Contains(contractVars, v.Name) || slices.Contains(optionalVars, v.Name) ||
			builtinTrueNAS && slices.Contains(truenasVars, v.Name)
	})
}

// writeModule writes the module's files and its variables into dir.
func writeModule(dir string, files map[string][]byte, vars map[string]any) error {
	for _, n := range sortedKeys(files) {
		if err := writeAtomic(filepath.Join(dir, n), bytes.NewReader(files[n]), 0o644, nil); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, varsFile), bytes.NewReader(append(b, '\n')), 0o644, nil)
}

// tofuOutputs reads the applied module's outputs: its `id`, required, and its `address`, empty
// when the module has none or the platform did not tell.
func (in *Installer) tofuOutputs(ctx context.Context, dir, ctl string) (id, addr string, err error) {
	env := append(in.O.environ(), CtlVar+"="+ctl, "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	out, err := in.O.Tofu.Run(ctx, Cmd{Args: []string{"-chdir=" + dir, "output", "-json", "-no-color"}, Env: env})
	if err != nil {
		return "", "", fmt.Errorf("tofu output: %w", err)
	}
	var o map[string]struct {
		Value any `json:"value"`
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber() // a numeric id kept exact, whatever its size
	if err := dec.Decode(&o); err != nil {
		return "", "", fmt.Errorf("tofu output: %w", err)
	}
	switch v := o["id"].Value.(type) {
	case nil:
		return "", "", errors.New("the module has no output id: a module `vm create` applies outputs the VM's id (and its address, when the platform tells it)")
	case string:
		id = strings.TrimSpace(v)
	case json.Number:
		id = v.String()
	default:
		return "", "", fmt.Errorf("the module's output id is %T: a module `vm create` applies outputs the VM's id as a string or a number", v)
	}
	if id == "" {
		return "", "", errors.New("the module's output id is empty: a module `vm create` applies outputs the VM's id")
	}
	if a, ok := o["address"].Value.(string); ok {
		addr = strings.TrimSpace(a)
	}
	return id, addr, nil
}

// readPlatform reads what a VM's state directory records of its platform; a state from before it
// was recorded is TrueNAS's built-in module's, the only one then.
func readPlatform(dir string) (key string, builtin bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, platformFile))
	if errors.Is(err, os.ErrNotExist) {
		return "truenas", true, nil
	}
	if err != nil {
		return "", false, err
	}
	key, how, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	return key, how == "builtin", nil
}
