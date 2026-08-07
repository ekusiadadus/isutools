#!/bin/sh
set -eu

if [ "$(stat -fc %T /sys/fs/cgroup)" != "cgroup2fs" ]; then
	echo "cgroup v2 is required" >&2
	exit 1
fi

self_path=$(awk -F: '$1 == "0" { print $3 }' /proc/self/cgroup)
self_parent=/sys/fs/cgroup${self_path}
parent=${ISUTOOLS_PPROF_TEST_CGROUP_PARENT:-$self_parent}
if [ ! -s "$parent/cgroup.controllers" ] && [ "$parent" = "$self_parent" ] && [ -w /sys/fs/cgroup ]; then
	# Docker commonly places the privileged fixture process in a leaf with no
	# controllers delegated to children. The host cgroup root is a safe sibling
	# fixture only when it was explicitly mounted writable into this container.
	parent=/sys/fs/cgroup
fi
delegated=${parent}/isutools-pprof-integration-$$
oom_helper=/tmp/isutools-pprof-oom-worker-$$

cleanup() {
	rm -f "$oom_helper"
	if [ -d "$delegated" ]; then
		rmdir "$delegated"
	fi
}
trap cleanup EXIT INT TERM

mkdir "$delegated"
controllers=$(cat "$delegated/cgroup.controllers")
case " $controllers " in
	*" memory "*" pids "*) ;;
	*)
		echo "delegated cgroup lacks memory/pids controllers: $controllers" >&2
		exit 1
		;;
esac
if ! printf '+memory +pids\n' >"$delegated/cgroup.subtree_control"; then
	echo "cannot enable controllers under $delegated" >&2
	echo "child type: $(cat "$delegated/cgroup.type")" >&2
	echo "parent subtree_control: $(cat "$parent/cgroup.subtree_control")" >&2
	echo "child processes: $(tr '\n' ' ' <"$delegated/cgroup.procs")" >&2
	exit 1
fi

cc -O2 -Wall -Wextra -Werror -o "$oom_helper" integration/pprof_oom_worker.c

ISUTOOLS_PPROF_TEST_CGROUP_ROOT="$delegated" \
	ISUTOOLS_PPROF_TEST_OOM_HELPER="$oom_helper" \
	go test ./internal/pprofanalyze -run 'TestLaunchWorkerLinuxCgroup(Bootstrap|RealRuntimeProfile|OOMLeavesParentUsable)' -count=1 -v
