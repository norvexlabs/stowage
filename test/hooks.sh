#!/usr/bin/env bash
# The hooks' branches against throwaway repositories: the pre-push hook's credential checks with a
# credential helper returning a chosen token, then the private-words check (.githooks/private-words)
# with a list holding a fake word, through pre-commit, commit-msg and pre-push (a push to a local
# bare repository). No network, no real credential, no real list. Each case checks the exit status
# and which refusal fired, and that the word never appears in the output. Run by test/gate.sh,
# after lint.
set -euo pipefail
hooks=$(cd "$(dirname "$0")/.." && pwd)/.githooks
hook=$hooks/pre-push
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT NETRC \
	GIT_COMMON_DIR GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE GIT_PREFIX \
	GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_AUTHOR_DATE GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL GIT_COMMITTER_DATE
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

# The private-words check. The fake word is split here so that a failure message quoting this
# script's own lines does not count as the hook printing it.
word=zebra"quill"
list=$XDG_CONFIG_HOME/norvexlabs/private-words
git init -q "$work/p"
cd "$work/p"
mkdir .githooks
cp "$hooks/identity" .githooks/
git config user.name "$(sed 's/ <.*//' .githooks/identity)"
git config user.email "$(sed 's/.*<\(.*\)>/\1/' .githooks/identity)"
pw() { # pw <reason substring, or "" for allowed> <name> <command...>
	local want=$1 name=$2 got=0
	shift 2
	"$@" >"$work/out" 2>&1 || got=1
	if [[ -z $want && $got != 0 ]]; then
		echo "FAIL $name: refused, want allowed: $(cat "$work/out")"
		fails=$((fails + 1))
	elif [[ -n $want ]] && ! { [[ $got == 1 ]] && grep -qF -- "$want" "$work/out"; }; then
		echo "FAIL $name: exit $got, want a refusal naming '$want': $(cat "$work/out")"
		fails=$((fails + 1))
	fi
	if grep -qi "$word" "$work/out"; then
		echo "FAIL $name: printed the private word"
		fails=$((fails + 1))
	fi
}
staged() { git rm -rq --cached . >/dev/null 2>&1 || true; git add "$@"; }
mkdir -p "$(dirname "$list")"

echo clean >a.txt
staged a.txt
pw "" "no list" sh "$hooks/pre-commit"
grep -q warning "$work/out" || { echo "FAIL no list: no warning"; fails=$((fails + 1)); }
printf '\n  \n' >"$list"
pw "" "a list with no pattern" sh "$hooks/pre-commit"
grep -q "holds no pattern" "$work/out" || { echo "FAIL no pattern: no warning"; fails=$((fails + 1)); }
printf 'nothing-here\n\n%s\n' "$word" >"$list"
pw "" "clean" sh "$hooks/pre-commit"
printf 'one\nan %s here\n' "$(echo "$word" | tr a-z A-Z)" >b.txt
staged a.txt b.txt
pw "file 2 (git ls-files --cached numbering), line 2" "content match, any case" sh "$hooks/pre-commit"
mkdir d && echo clean >"d/x-$word.txt"
staged a.txt d
pw "the path of file 2" "path match" sh "$hooks/pre-commit"
printf 'the %s\n' "$word" >"c:$word:1.txt"
staged a.txt "c:$word:1.txt"
pw "file 2 (git ls-files --cached numbering), line 1" "a matching path with colons" sh "$hooks/pre-commit"
printf 'x\n%s:2: text\n' "$word" >"e:1:z.txt"
staged a.txt "e:1:z.txt"
pw "file 2 (git ls-files --cached numbering), line 2" "a path with colons" sh "$hooks/pre-commit"
printf '\0\1%s\0' "$word" >bin.dat
staged a.txt bin.dat
pw "file 2 (git ls-files --cached numbering), line 1" "binary file" sh "$hooks/pre-commit"
staged a.txt
printf '%s\n(%s\n' "$word" "$word" >"$list"
pw "not a valid regex: line 2" "bad regex" sh "$hooks/pre-commit"
rm "$list" && mkdir "$list"
pw "cannot be read" "a directory as the list" sh "$hooks/pre-commit"
rmdir "$list" && ln -s "$work/none" "$list"
pw "cannot be read" "a dangling symlink as the list" sh "$hooks/pre-commit"
rm "$list"
if [[ $(id -u) != 0 ]]; then
	printf '%s\n' "$word" >"$list" && chmod 000 "$list"
	pw "cannot be read" "an unreadable list" sh "$hooks/pre-commit"
	rm -f "$list"
fi
printf '%s\r\n' "$word" >"$list"
printf 'the %s\n' "$word" >b.txt
staged a.txt b.txt
pw "file 2 (git ls-files --cached numbering), line 1" "a list with CRLF line ends" sh "$hooks/pre-commit"
printf 'caf\303\251\n' >"$list"
echo clean >"d-caf$(printf '\303\251').txt"
staged a.txt "d-caf$(printf '\303\251').txt"
pw "the path of file 2" "a non-ASCII word in a path" sh "$hooks/pre-commit"
echo clean >"D-CAF$(printf '\303\211').txt"
staged a.txt "D-CAF$(printf '\303\211').txt"
pw "the path of file 1" "a non-ASCII word in another case" sh "$hooks/pre-commit"
staged a.txt
printf '%s\n' "$word" >"$list"
printf 'Subject\n\nBody with %s\n' "$word" >"$work/msg"
pw "the commit message, line 3" "message match" sh "$hooks/commit-msg" "$work/msg"
printf 'Subject\n\nBody\n' >"$work/msg"
pw "" "clean message" sh "$hooks/commit-msg" "$work/msg"

# Pushes to a local bare repository, through the hooks; commits made with plumbing, no hook.
git init -q --bare "$work/bare.git"
commit() { # commit <message> <parent or ""> → sha, of the index
	local t
	t=$(git write-tree)
	if [[ -n $2 ]]; then git commit-tree -m "$1" -p "$2" "$t"; else git commit-tree -m "$1" "$t"; fi
}
push() { git -c core.hooksPath="$hooks" push -q "$work/bare.git" "$1:refs/heads/main"; }
staged a.txt
c1=$(commit "first" "")
pw "" "clean push, a new branch" push "$c1"
printf 'the %s\n' "$word" >b.txt
staged a.txt b.txt
c2=$(commit "second" "$c1")
staged a.txt
c3=$(commit "third" "$c2")
pw "commit $(git rev-parse --short "$c2"): file 2 (git ls-tree -r $(git rev-parse --short "$c2") numbering), line 1" "content in a pushed commit" push "$c3"
c4=$(commit "fourth, $word" "$c1")
pw "its object (git cat-file commit" "message of a pushed commit" push "$c4"
c5=$(commit "fifth" "$c1")
pw "" "clean push onto the branch" push "$c5"
[[ $(git --git-dir="$work/bare.git" rev-parse main) == "$c5" ]] || { echo "FAIL clean push: main is not the pushed commit"; fails=$((fails + 1)); }
git tag -a -m "tag of $word" t1 "$c5"
pw "the pushed tag object" "an annotated tag's message" git -c core.hooksPath="$hooks" push "$work/bare.git" refs/tags/t1
git tag -a -m "inner $word" t2 "$c5"
git -c advice.nestedTag=false tag -a -m "outer, clean" t3 t2
pw "the pushed tag object $(git rev-parse --short t2)" "a nested tag's inner message" git -c core.hooksPath="$hooks" push "$work/bare.git" refs/tags/t3
c8=$(GIT_AUTHOR_NAME="A $word" git commit-tree -m "eighth" -p "$c5" "$(git write-tree)")
pw "its object (git cat-file commit" "a pushed commit's author" push "$c8"
pw "a pushed ref's name" "a pushed ref's name" git -c core.hooksPath="$hooks" push "$work/bare.git" "$c5:refs/heads/x-$word"
echo side >s.txt
staged a.txt s.txt
m1=$(commit "side" "$c1")
staged a.txt b.txt s.txt
m2=$(git commit-tree -m "merge" -p "$c5" -p "$m1" "$(git write-tree)")
pw "commit $(git rev-parse --short "$m2"): file 2" "content a merge brings" push "$m2"
# What another remote has is checked again: a commit a mirror has, pushed to a new branch.
git update-ref refs/remotes/mirror/main "$c2"
pw "commit $(git rev-parse --short "$c2"): file 2" "a commit another remote has" git -c core.hooksPath="$hooks" push -q "$work/bare.git" "$c2:refs/heads/new"
git update-ref -d refs/remotes/mirror/main
# What the pushed-to remote has is not checked again: a new branch on a commit its refs hold.
git remote add origin "$work/bare.git"
git update-ref refs/remotes/origin/has "$c2"
c9=$(commit "ninth" "$c2")
pw "" "a commit the remote has, under a new branch" git -c core.hooksPath="$hooks" push -q origin "$c9:refs/heads/nine"
pw "" "a deletion" git -c core.hooksPath="$hooks" push -q origin :refs/heads/nine
git update-ref -d refs/remotes/origin/has
# A remote sha this clone lacks: every commit no ref of the remote has is checked.
git update-ref refs/heads/keep "$c3" # a local ref holds them: still checked
printf 'refs/heads/main %s refs/heads/main %s\n' "$c3" 1234567890123456789012345678901234567890 >"$work/in"
pw "commit $(git rev-parse --short "$c2"): file 2" "an unknown remote sha" sh "$hook" origin "$work/bare.git" <"$work/in"
# A blob missing here (a partial clone) refuses.
printf 'blob only here %s\n' "$RANDOM$RANDOM" >g.txt
staged a.txt g.txt
c7=$(commit "seventh" "$c5")
b=$(git rev-parse "$c7:g.txt")
rm ".git/objects/${b:0:2}/${b:2}"
pw "cannot be read" "a missing blob" push "$c7"
staged a.txt
rm "$list"
staged a.txt b.txt
c6=$(commit "sixth" "$c5")
pw "" "a push with no list" push "$c6"

if ((fails)); then
	echo "hooks: $fails failed" >&2
	exit 1
fi
echo "hooks: PASS"
