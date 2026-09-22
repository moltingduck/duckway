#!/bin/sh
# Keep Git's repository-local environment from leaking into quality checks.
# Some tests invoke git in a child process; those variables must not select a
# temporary repository for the checks that follow.
duckway_prepare_precommit() {
	DUCKWAY_PRECOMMIT_ROOT=$(git rev-parse --show-toplevel)
	for name in $(git rev-parse --local-env-vars); do
		unset "$name"
	done
	cd "$DUCKWAY_PRECOMMIT_ROOT"
}
