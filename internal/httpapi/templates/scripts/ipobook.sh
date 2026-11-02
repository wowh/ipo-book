#!/bin/sh
# 打新账本本地接口的调用脚本。
#
#   ./ipobook.sh health
#   ./ipobook.sh doctor          # 看不到接口时先跑这个：它打印找到的文件与地址
#   ./ipobook.sh get "/api/issues?scope=issuing&limit=10"
#   ./ipobook.sh get /api/entries
#   ./ipobook.sh post /api/entries '{"brokerId":1,"fxCode":"920306","applyShares":1000}'
#   ./ipobook.sh patch /api/entries/12 '{"allottedShares":500}'
#   ./ipobook.sh sell 12 '{"date":"2026-10-28","shares":400,"price":"6.20"}'   # 追加一笔卖出
#   ./ipobook.sh stats "year=2026&month=10"     # 按年、按月、按券商看收益
#   ./ipobook.sh delete /api/entries/12
#   ./ipobook.sh refresh [scope]
#   ./ipobook.sh skill [dir]
#   ./ipobook.sh raw GET /api/summary
#
# 接口地址与令牌从应用写下的 {{discoveryName}} 读取。
#
# 端口可能被占用而换成随机端口，数据目录也可能被改到别处，所以这里按顺序找：
#   1. $IPO_BOOK_API 指定的文件
#   2. ~/.ipo-book/api.json（应用写的固定指针，推荐）
#   3. 数据目录里的发现文件（各平台默认位置）
# 找到之后先探一次 /api/health，只有真能应答的才算数，避免捡到崩溃留下的旧文件。
#   IPO_BOOK_API=/path/to/api.json ./ipobook.sh health
set -eu

DEFAULT_DISCOVERY="{{discoveryPath}}"

die() {
    echo "$*" >&2
    exit 1
}

# candidates 列出所有可能写着接入信息的文件，一行一个。
candidates() {
    printf '%s\n' "${IPO_BOOK_API:-}"
    printf '%s\n' "${HOME:-/nonexistent}/.ipo-book/api.json"
    printf '%s\n' "$DEFAULT_DISCOVERY"
    printf '%s\n' "${HOME:-/nonexistent}/Library/Application Support/bse-ipo-book/api.json"
    printf '%s\n' "${XDG_CONFIG_HOME:-${HOME:-/nonexistent}/.config}/bse-ipo-book/api.json"
    printf '%s\n' "${APPDATA:-/nonexistent}/bse-ipo-book/api.json"
}

# json_field 按字段名取值。发现文件由程序生成，一行一个字段。
json_field() {
    sed -n "s/.*\"$1\": *\"\([^\"]*\)\".*/\1/p" "$2" | head -n 1
}

# probe 判断这个地址上是否真有一个能应答的账本接口。
probe() {
    curl -sf --noproxy '*' --max-time 3 \
        -H "Authorization: Bearer $2" "$1/api/health" >/dev/null 2>&1
}

# find_endpoint 按顺序找出第一个能用的接入信息，设置 DISC / BASE / TOKEN。
find_endpoint() {
    tried=""
    seen=""
    old_ifs=$IFS
    IFS='
'
    # 在函数里重排位置参数，不会影响脚本自己的参数。
    set -- $(candidates)
    IFS=$old_ifs
    for candidate in "$@"; do
        [ -n "$candidate" ] || continue
        # 默认数据目录与平台候选常是同一个路径，重复的只报一次。
        case "$seen" in
            *"|$candidate|"*) continue ;;
        esac
        seen="$seen|$candidate|"
        if [ ! -f "$candidate" ]; then
            tried="$tried
    ${candidate}（不存在）"
            continue
        fi
        base=$(json_field baseUrl "$candidate")
        token=$(json_field token "$candidate")
        if [ -z "$base" ] || [ -z "$token" ]; then
            tried="$tried
    ${candidate}（读不出地址或令牌）"
            continue
        fi
        if ! probe "$base" "$token"; then
            tried="$tried
    ${candidate}（${base} 没有应答）"
            continue
        fi
        DISC=${candidate}
        BASE=$base
        TOKEN=$token
        return 0
    done
    return 1
}

DISC=""
BASE=""
TOKEN=""
if ! find_endpoint; then
    die "没有找到在运行的「打新账本」。请先打开应用，然后重试。已找过：$tried
也可以用 IPO_BOOK_API 指定发现文件的位置。"
fi

# pretty 把 JSON 排版并保留中文，没有 python3 就原样输出。
pretty() {
    if command -v python3 >/dev/null 2>&1; then
        python3 -c "import json,sys; print(json.dumps(json.load(sys.stdin), ensure_ascii=False, indent=2))" 2>/dev/null && return 0
    fi
    cat
}

# req 发请求并打印响应；非 2xx 时返回非零。
#
# 接口只在本机，显式绕过代理：有的机器把 HTTP_PROXY 指向公司代理，
# 不绕过的话连 127.0.0.1 也会被代理转发出去，直接报连接失败。
req() {
    method=$1
    path=$2
    body=${3:-}
    if [ -n "$body" ]; then
        response=$(curl -sS --noproxy '*' -X "$method" \
            -H "Authorization: Bearer $TOKEN" \
            -H 'Content-Type: application/json' \
            --data "$body" "$BASE$path" -w '\n%{http_code}')
    else
        response=$(curl -sS --noproxy '*' -X "$method" \
            -H "Authorization: Bearer $TOKEN" \
            "$BASE$path" -w '\n%{http_code}')
    fi
    code=$(printf '%s' "$response" | tail -n 1)
    payload=$(printf '%s' "$response" | sed '$d')
    printf '%s\n' "$payload" | pretty
    case "$code" in
        2*) return 0 ;;
        *)
            echo "HTTP $code" >&2
            return 1
            ;;
    esac
}

cmd=${1:-health}
shift || true
case "$cmd" in
    health)  req GET /api/health ;;
    doctor)  echo "发现文件 $DISC"; echo "接口地址 $BASE"; req GET /api/health ;;
    get)     [ $# -ge 1 ] || die "用法：ipobook.sh get /api/entries"; req GET "$1" ;;
    post)    [ $# -ge 2 ] || die "用法：ipobook.sh post /api/entries '{...}'"; req POST "$1" "$2" ;;
    patch)   [ $# -ge 2 ] || die "用法：ipobook.sh patch /api/entries/12 '{...}'"; req PATCH "$1" "$2" ;;
    sell)    [ $# -ge 2 ] || die "用法：ipobook.sh sell 12 '{\"date\":\"2026-10-28\",\"shares\":400,\"price\":\"6.20\"}'"
             req POST "/api/entries/$1/sales" "$2" ;;
    delete)  [ $# -ge 1 ] || die "用法：ipobook.sh delete /api/entries/12"; req DELETE "$1" ;;
    stats)   [ $# -ge 1 ] || die "用法：ipobook.sh stats 'year=2026&month=10&brokerId=1'"
             req GET "/api/stats?$1" ;;
    refresh) req POST /api/issues/refresh "{\"scope\":\"${1:-all}\"}" ;;
    skill)   req POST /api/skill/export "{\"dir\":\"${1:-}\"}" ;;
    raw)     [ $# -ge 2 ] || die "用法：ipobook.sh raw GET /api/summary"; req "$@" ;;
    *)       die "未知命令 ${cmd}，可用：health doctor get post patch sell stats delete refresh skill raw" ;;
esac
