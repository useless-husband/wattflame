#!/bin/sh
# Compare the energy wattflame attributes to each phase of examples/validate
# with the energy the kernel itself reports for that phase.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
wattflame=${WATTFLAME:-"$here/../wattflame"}
seconds=${1:-1.5}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

"$wattflame" record -q -no-html -o "$tmp/validate.json" -- "$here/bin/validate" "$seconds" > "$tmp/truth.txt"
"$wattflame" report -folded - "$tmp/validate.json" > "$tmp/folded.txt"

printf '%-18s %12s %12s %9s\n' PHASE KERNEL WATTFLAME DIFFERENCE
awk '
	NR == FNR { truth[$2] = $3; order[++n] = $2; next }
	{
		value = $NF
		for (i = 1; i <= n; i++) {
			if (index($0, ";" order[i] ";") || index($0, ";" order[i] " ")) got[order[i]] += value
		}
	}
	END {
		for (i = 1; i <= n; i++) {
			p = order[i]
			diff = truth[p] > 0 ? 100 * (got[p] - truth[p]) / truth[p] : 0
			printf "%-18s %9.1f mJ %9.1f mJ %+8.1f%%\n", p, truth[p] / 1000, got[p] / 1000, diff
			sum_truth += truth[p]; sum_got += got[p]
		}
		printf "%-18s %9.1f mJ %9.1f mJ %+8.1f%%\n", "all phases", sum_truth / 1000, sum_got / 1000, 100 * (sum_got - sum_truth) / sum_truth
	}
' "$tmp/truth.txt" "$tmp/folded.txt"
