#!/usr/bin/env bash

. "test/testlib.sh"

begin_test "unlocking a lock by path"
(
  set -e

  setup_remote_repo_with_file "unlock_by_path" "c.dat"

  GITLFSLOCKSENABLED=1 git lfs lock "c.dat" | tee lock.log

  id=$(grep -oh "\((.*)\)" lock.log | tr -d "()")
  assert_server_lock $id

  GITLFSLOCKSENABLED=1 git lfs unlock "c.dat" 2>&1 | tee unlock.log
  refute_server_lock $id
)
end_test

begin_test "unlocking a lock by id"
(
  set -e

  setup_remote_repo_with_file "unlock_by_id" "d.dat"

  GITLFSLOCKSENABLED=1 git lfs lock "d.dat" | tee lock.log

  id=$(grep -oh "\((.*)\)" lock.log | tr -d "()")
  assert_server_lock $id

  GITLFSLOCKSENABLED=1 git lfs unlock --id="$id" 2>&1 | tee unlock.log
  refute_server_lock $id
)
end_test

begin_test "unlocking a lock without sufficient info"
(
  set -e

  setup_remote_repo_with_file "unlock_ambiguous" "e.dat"

  GITLFSLOCKSENABLED=1 git lfs lock "e.dat" | tee lock.log

  id=$(grep -oh "\((.*)\)" lock.log | tr -d "()")
  assert_server_lock $id

  GITLFSLOCKSENABLED=1 git lfs unlock 2>&1 | tee unlock.log
  grep "Usage: git lfs unlock" unlock.log
  assert_server_lock $id
)
end_test
