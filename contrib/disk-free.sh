#!/bin/sh
# disk-free.sh — a zmina exec watch source: one JSON event per filesystem whose free space is under
# a threshold. Portable (df -P, awk), macOS and Linux.
#
# The fingerprint is the free share rounded down to the step, so the dispatcher hears once when a
# breach starts and again only when it worsens by another step; back above the threshold, the key
# disappears and a later breach is news again.
#
#   watch:
#     disk:
#       kind: exec
#       every: 5m
#       baseline: false                  # already low at the first run -> say so
#       run: [sh, /path/to/zmina/contrib/disk-free.sh, --min, "15", /, /home]
#
# Flags: --min <percent free> (default 15), --step <percent> (default 5); then the mount points
# to check (default /).

min=15
step=5

while [ $# -gt 0 ]; do
	case $1 in
	--min) min=$2; shift 2 ;;
	--step) step=$2; shift 2 ;;
	--) shift; break ;;
	-*) echo "disk-free.sh: unknown flag $1" >&2; exit 2 ;;
	*) break ;;
	esac
done

[ $# -gt 0 ] || set -- /

df -Pk "$@" | awk -v min="$min" -v step="$step" 'NR > 1 {
	total = $2; avail = $4; mount = $6
	for (i = 7; i <= NF; i++) mount = mount " " $i
	if (total <= 0) next
	free = 100 * avail / total
	if (free >= min) next
	bucket = int(free / step) * step
	gsub(/["\\]/, "", mount)
	printf "{\"key\":\"disk:%s\",\"fingerprint\":\"%d\",\"text\":\"disk %s: %.0f%% free (%.0f GB of %.0f)\"}\n",
		mount, bucket, mount, free, avail / 1048576, total / 1048576
}'
