package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"

	"github.com/norvexlabs/stowage/internal/spec"
)

// FrontLogins holds the bcrypt hash of each route login's password (0600): a hash is salted, so
// one made at each start would change the front's configuration, and restart it, every time.
// A password that changed gets a new hash.
const FrontLogins = ".stowage-front-logins"

// login is a route's basic_auth: its user, and the bcrypt hash of its password; no hash while the
// password is empty, and the route then refuses everyone.
type login struct {
	User, Hash string
}

// maxLoginPassword is the longest password bcrypt takes, in bytes.
const maxLoginPassword = 72

// checkLoginPassword refuses a route login's password bcrypt cannot take.
func checkLoginPassword(v string) error {
	if len(v) > maxLoginPassword {
		return fmt.Errorf("is %d bytes, over the %d bcrypt takes", len(v), maxLoginPassword)
	}
	return nil
}

// logins are the front's routes' logins (basic_auth), by route, their passwords read with get,
// for a stack rendered into dir: each hash dir's FrontLogins holds (the install's, for another
// dir without one: the release kept for a rollback) that still matches its password is kept, and
// dir's file is written again when one changed. A password bcrypt cannot take leaves its route
// refusing everyone, logged.
func (in *Installer) logins(f spec.Front, get func(string) string, dir string) (map[string]login, error) {
	out := map[string]login{}
	kept := map[string]string{}
	path := filepath.Join(dir, FrontLogins)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && dir != in.O.Dir {
		var from []byte
		if from, err = os.ReadFile(in.path(FrontLogins)); err == nil && json.Unmarshal(from, &kept) != nil {
			kept = map[string]string{}
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(raw) > 0 && json.Unmarshal(raw, &kept) != nil {
		kept = map[string]string{} // made again
	}
	hashes := map[string]string{}
	for _, r := range f.Routes {
		a := r.BasicAuth
		if a == nil {
			continue
		}
		pw := get(a.Secret)
		if pw == "" {
			out[r.Name] = login{User: a.User}
			continue
		}
		if err := checkLoginPassword(pw); err != nil {
			in.O.logf("route %s's password (%s) %v: the route refuses everyone until it is shorter", r.Name, a.Secret, err)
			out[r.Name] = login{User: a.User}
			continue
		}
		h := kept[r.Name]
		if bcrypt.CompareHashAndPassword([]byte(h), []byte(pw)) != nil {
			made, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
			if err != nil {
				return nil, fmt.Errorf("route %s's password (%s): %w", r.Name, a.Secret, err)
			}
			h = string(made)
		}
		hashes[r.Name] = h
		out[r.Name] = login{User: a.User, Hash: h}
	}
	next, err := json.Marshal(hashes)
	if err != nil {
		return nil, err
	}
	switch {
	case len(hashes) == 0:
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	case !bytes.Equal(raw, next):
		if err := writeAtomic(path, bytes.NewReader(next), 0o600, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}
