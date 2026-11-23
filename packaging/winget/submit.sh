#!/bin/sh
# Pushes a rendered WinGet manifest set to a branch of the nickheyer/winget-pkgs fork, opens its pull request on microsoft/winget-pkgs and closes the open ones it supersedes
set -eu
dir=${1:?usage: submit.sh <manifest dir>}
: "${WINGET_TOKEN:?submit: WINGET_TOKEN is not set}"
: "${GITHUB_SERVER_URL:?submit: GITHUB_SERVER_URL is not set}" "${GITHUB_REPOSITORY:?submit: GITHUB_REPOSITORY is not set}" "${GITHUB_RUN_ID:?submit: GITHUB_RUN_ID is not set}"
api=${GITHUB_API_URL:-https://api.github.com}
id=nickheyer.icebath
fork=nickheyer/winget-pkgs
upstream=microsoft/winget-pkgs
base=master
version=$(awk 'index($0, "PackageVersion: ") == 1 { v = substr($0, 17); gsub(/^"|"$/, "", v); print v; exit }' "$dir/$id.yaml")
[ -n "$version" ] || { echo "submit: $dir/$id.yaml names no PackageVersion" >&2; exit 1; }
branch=icebath-$version
package=manifests/n/nickheyer/icebath
path=$package/$version
body=$(mktemp)
request=$(mktemp)
trap 'rm -f "$body" "$request"' EXIT

# Sends one API request with an optional JSON body, leaving the response in $body and the status in $code
call() {
    if [ $# -eq 2 ]; then
        code=$(curl -sS -o "$body" -w '%{http_code}' -H "Authorization: Bearer $WINGET_TOKEN" -H 'Accept: application/vnd.github+json' \
            -H 'X-GitHub-Api-Version: 2022-11-28' -X "$1" "$api$2")
    else
        printf '%s' "$3" > "$request"
        code=$(curl -sS -o "$body" -w '%{http_code}' -H "Authorization: Bearer $WINGET_TOKEN" -H 'Accept: application/vnd.github+json' \
            -H 'X-GitHub-Api-Version: 2022-11-28' -H 'Content-Type: application/json' -X "$1" --data-binary @"$request" "$api$2")
    fi
}

# Stops the submission with the request, its status and the API message
refuse() {
    printf 'submit: %s %s answered %s: %s\n' "$1" "$2" "$code" "$(jq -r '.message // empty' "$body" 2>/dev/null || cat "$body")" >&2
    exit 1
}

# Closes every other open pull request of this package that the token's account opened, naming the one that replaces it
supersede() {
    call GET "/user"
    [ "$code" = 200 ] || refuse GET "/user"
    login=$(jq -r '.login' "$body")
    query=$(printf 'repo:%s is:pr is:open author:%s in:title %s' "$upstream" "$login" "$id" | jq -sRr '@uri')
    call GET "/search/issues?q=$query&per_page=100"
    [ "$code" = 200 ] || refuse GET "/search/issues"
    for other in $(jq -r --argjson n "$1" '.items[].number | select(. != $n)' "$body"); do
        call POST "/repos/$upstream/issues/$other/comments" "$(jq -n --arg b "Superseded by #$1" '{body: $b}')"
        [ "$code" = 201 ] || refuse POST "/repos/$upstream/issues/$other/comments"
        call PATCH "/repos/$upstream/pulls/$other" '{"state":"closed"}'
        [ "$code" = 200 ] || refuse PATCH "/repos/$upstream/pulls/$other"
        echo "submit: closed pull request #$other, superseded by #$1"
    done
}

call GET "/repos/$upstream/contents/$path?ref=$base"
case $code in
    200) echo "submit: $upstream $base already has $path, nothing to submit"; exit 0 ;;
    404) ;;
    *) refuse GET "/repos/$upstream/contents/$path" ;;
esac

call GET "/repos/$upstream/contents/$package?ref=$base"
case $code in
    200) kind='New version' ;;
    404) kind='New package' ;;
    *) refuse GET "/repos/$upstream/contents/$package" ;;
esac
msg="$kind: $id version $version"

call GET "/repos/$fork"
[ "$code" = 200 ] || refuse GET "/repos/$fork"
[ "$(jq -r '.fork' "$body")" = true ] && [ "$(jq -r '.parent.full_name' "$body")" = "$upstream" ] ||
    { echo "submit: $fork is not a fork of $upstream, run scripts/release/setup.sh" >&2; exit 1; }
default=$(jq -r '.default_branch' "$body")

call POST "/repos/$fork/merge-upstream" "$(jq -n --arg b "$base" '{branch: $b}')"
[ "$code" = 200 ] || refuse POST "/repos/$fork/merge-upstream"
echo "submit: $fork $base synced with $upstream ($(jq -r '.merge_type' "$body"))"

call GET "/repos/$fork/branches/$branch"
case $code in
    200) echo "submit: branch $branch exists on $fork" ;;
    404)
        call GET "/repos/$fork/git/ref/heads/$default"
        [ "$code" = 200 ] || refuse GET "/repos/$fork/git/ref/heads/$default"
        sha=$(jq -r '.object.sha' "$body")
        call POST "/repos/$fork/git/refs" "$(jq -n --arg r "refs/heads/$branch" --arg s "$sha" '{ref: $r, sha: $s}')"
        if [ "$code" = 201 ]; then
            echo "submit: created branch $branch on $fork at $sha"
        elif [ "$code" != 422 ] || [ "$(jq -r '.message' "$body")" != "Reference already exists" ]; then
            refuse POST "/repos/$fork/git/refs"
        fi
        ;;
    *) refuse GET "/repos/$fork/branches/$branch" ;;
esac

for name in "$id.yaml" "$id.installer.yaml" $(cd "$dir" && ls "$id".locale.*.yaml); do
    case $name in
        *.installer.yaml) part=installer ;;
        *.locale.*.yaml) part=locale ;;
        *) part=version ;;
    esac
    file=$dir/$name
    call GET "/repos/$fork/contents/$path/$name?ref=$branch"
    case $code in
        200)
            if [ "$(jq -r '.content' "$body" | base64 -d | sha256sum)" = "$(sha256sum < "$file")" ]; then
                echo "submit: $path/$name is already current on $branch"
                continue
            fi
            sha=$(jq -r '.sha' "$body")
            ;;
        404) sha= ;;
        *) refuse GET "/repos/$fork/contents/$path/$name" ;;
    esac
    call PUT "/repos/$fork/contents/$path/$name" "$(jq -n --arg m "$msg: add $part" --arg c "$(base64 -w0 < "$file")" --arg b "$branch" --arg s "$sha" \
        '{message: $m, content: $c, branch: $b, committer: {name: "nickheyer", email: "nick@heyer.app"}} + (if $s == "" then {} else {sha: $s} end)')"
    case $code in
        200 | 201) echo "submit: pushed $path/$name to $branch" ;;
        *) refuse PUT "/repos/$fork/contents/$path/$name" ;;
    esac
done

owner=${fork%%/*}
call GET "/repos/$upstream/pulls?state=open&head=$owner:$branch&base=$base"
[ "$code" = 200 ] || refuse GET "/repos/$upstream/pulls"
number=$(jq -r '.[0].number // empty' "$body")
if [ -n "$number" ]; then
    echo "submit: pull request already open: $(jq -r '.[0].html_url' "$body")"
    supersede "$number"
    exit 0
fi

call GET "/repos/$upstream/contents/.github/PULL_REQUEST_TEMPLATE.md?ref=$base"
case $code in
    200) template=$(jq -r '.content' "$body" | base64 -d) ;;
    404) template= ;;
    *) refuse GET "/repos/$upstream/contents/.github/PULL_REQUEST_TEMPLATE.md" ;;
esac
footer="###### Automated with the icebath release workflow ($GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID)"
call POST "/repos/$upstream/pulls" "$(jq -n --arg t "$msg" --arg h "$owner:$branch" --arg b "$base" --arg d "$template
$footer" '{title: $t, head: $h, base: $b, body: $d, draft: false}')"
[ "$code" = 201 ] || refuse POST "/repos/$upstream/pulls"
number=$(jq -r '.number' "$body")
echo "submit: opened $(jq -r '.html_url' "$body")"
supersede "$number"
