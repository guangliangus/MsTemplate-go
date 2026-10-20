#!/usr/bin/env bash
# Reports every leftover of the orders reference example after it was replaced
# (docs/replacing-the-example.md): a case-insensitive grep for `\border|inventory|sku\b` over the
# repository's Go, YAML, SQL, HTTP, shell, JSON and JSONC files. Prints each hit as file:line:text and
# exits 1 while any remain, 0 when the tree is clean.
#
# Usage: tools/check-example-removed.sh [--context <name>]...
#   --context <name>  a directory of the service's OWN context whose name happens to match
#                     (a real `inventory` context, say); it is skipped by directory name.
#
# Skipped on purpose: .git and bin, this script and docs/replacing-the-example.md (both name the
# example), tools/new-service.sh (the template's generator; its usage text names the example),
# db/kit_*.sql (byte-identical copies of the kit's scripts; tests/arch/kitddl_test.go forbids
# editing them). A bare English `order` or `ORDER` as a plain word (start order, stop order,
# ORDER BY) is not a hit either: the example only ever shows up as an identifier — `orders`,
# `order.`, `/order`, `"order"`, `order_`, `order-`, `Order…`, `ORDER_…` — and every repository's
# comments and SQL say the plain word.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PATTERN='\border|inventory|sku\b'

exclude_dirs=(.git bin)
while (($# > 0)); do
  case "$1" in
    --context)
      [[ $# -ge 2 ]] || { echo "--context needs a name" >&2; exit 2; }
      exclude_dirs+=("$2")
      shift 2
      ;;
    -h|--help)
      sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

grep_args=(-rnIiE "$PATTERN")
for d in "${exclude_dirs[@]}"; do grep_args+=("--exclude-dir=$d"); done
for ext in go yaml yml sql http sh json jsonc; do grep_args+=("--include=*.$ext"); done
grep_args+=(--exclude=check-example-removed.sh --exclude=new-service.sh --exclude='kit_*.sql')

# grep exits 1 when nothing matches; that is the clean case, not an error.
raw=$(cd "$ROOT" && grep "${grep_args[@]}" . || true)

# Drop the lines whose only matches are the plain word: `order`/`ORDER` not glued to identifier
# punctuation (a `.` counts only when an identifier follows it: `order.Service` is one, `order.`
# ending a sentence is not).
FILTER=$(cat <<'PERL'
my $line = $_;
(my $t = $line) =~ s/(?<![\w\/"'`_-])(?<!\w\.)(?:order|ORDER)(?!\.\w|[\w\/"'`_-])//g;
print $line if $t =~ /\border|inventory|sku\b/i;
PERL
)
hits=$(printf '%s\n' "$raw" | perl -ne "$FILTER" | sed 's#^\./##' | sort)

if [[ -n "$hits" ]]; then
  printf '%s\n' "$hits"
  count=$(printf '%s\n' "$hits" | wc -l | tr -d ' ')
  echo "example leftovers: $count line(s) still mention order/inventory/sku (docs/replacing-the-example.md)" >&2
  exit 1
fi
echo "no leftovers of the orders example"
