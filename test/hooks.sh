#!/usr/bin/env bash
# The pre-push hook's branches against a throwaway repository whose credential helper returns a
# chosen token; no network, no real credential. Each case checks the exit status and which
# refusal fired. Run by test/gate.sh when .githooks/ changes.
set -euo pipefail
hook=$(cd "$(dirname "$0")/.." && pwd)/.githooks/pre-push
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT NETRC
export HOME=$work XDG_CONFIG_HOME=$work GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
git init -q "$work/r"
cd "$work/r"
fails=0
helper() { git config --unset-all credential.helper 2>/dev/null || true; [[ -z $1 ]] || git config credential.helper "!f(){ test \"\$1\" = get && { echo username=x; echo password=$1; }; true; };f"; }
expect() { # expect <reason substring, or "" for allowed> <name> <url> [VAR=value ...]
	local want=$1 name=$2 url=$3 got=0
	shift 3
	env "$@" sh "$hook" origin "$url" </dev/null >"$work/err" 2>&1 || got=1
	if [[ -z $want && $got != 0 ]]; then
		echo "FAIL $name: refused, want allowed: $(cat "$work/err")"
		fails=$((fails + 1))
	elif [[ -n $want ]] && ! { [[ $got == 1 ]] && grep -qF -- "$want" "$work/err"; }; then
		echo "FAIL $name: exit $got, want a refusal naming '$want': $(cat "$work/err")"
		fails=$((fails + 1))
	elif grep -q SECRET "$work/err"; then
		echo "FAIL $name: printed the URL's userinfo"
		fails=$((fails + 1))
	fi
}
gh=https://github.com/org/repo.git
helper ghs_app
expect "" "app token" $gh
expect "" "upper-case scheme" HTTPS://github.com/org/repo.git
expect "" "harmless extraheader" $gh GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraheader GIT_CONFIG_VALUE_0="X-Foo: y"
expect "" "file url, lower case" file:///srv/repo.git
expect "" "absolute path" /srv/repo.git
expect "" "relative path" repo.git
expect "" "dot path" ../repo.git
expect "" "file url" FILE:///srv/repo.git
expect "personal credential" "personal token in the url" https://x:gho_SECRET@github.com/org/repo.git
expect "SSH (scp-like)" "ssh scp-like" git@github.com:org/repo.git
expect "SSH (scp-like)" "ssh host alias" gh-personal:org/repo.git
expect "not https" "ssh url" ssh://git@github.com/org/repo.git
expect "not https" "plain http" http://github.com/org/repo.git
expect "extraheader" "extraheader" $gh GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.https://github.com/.extraheader GIT_CONFIG_VALUE_0="AUTHORIZATION: bearer x"
expect "cookieFile" "cookie file" $gh GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.cookieFile GIT_CONFIG_VALUE_0=/c
expect "personal credential" "helper replaced by GIT_CONFIG_*" $gh GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0= \
	GIT_CONFIG_KEY_1=credential.helper GIT_CONFIG_VALUE_1="!f(){ echo username=x; echo password=gho_user; };f"
printf 'default login u password gho_x\n' >"$HOME/.netrc"
expect "netrc" "netrc default entry" $gh
expect "netrc" "netrc while NETRC points elsewhere" $gh NETRC="$work/none"
rm "$HOME/.netrc"
printf 'machine github.com login u password gho_x\n' >"$work/n"
expect "netrc" "NETRC" $gh NETRC="$work/n"
cp "$work/n" "$HOME/_netrc"
expect "netrc" "_netrc" $gh
rm "$HOME/_netrc"
expect "remote.*.vcs" "remote helper" $gh GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=remote.origin.vcs GIT_CONFIG_VALUE_0=gh
expect "HOME is unset" "HOME unset" $gh -u HOME
helper gho_user
expect "personal credential" "personal token" $gh
helper ""
expect "no credential" "no credential" $gh
if ((fails)); then
	echo "hooks: $fails failed" >&2
	exit 1
fi
echo "hooks: PASS"
