#!/bin/sh

# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Builds the User-Mode Linux kernel that the synctest demos boot.
#
# Usage: build.sh <linux-source-dir> <output-dir>
#
# The build is out of tree: the kernel source directory is not
# modified. The result is <output-dir>/linux, an ordinary executable.
# Pass its path to the demos in the UML_KERNEL environment variable.

set -eu

if [ $# -ne 2 ]; then
  echo "usage: $0 <linux-source-dir> <output-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
src=$(cd "$1" && pwd)
mkdir -p "$2"
out=$(cd "$2" && pwd)

make -C "$src" ARCH=um O="$out" x86_64_defconfig >/dev/null
(cd "$src" && ARCH=um scripts/kconfig/merge_config.sh -O "$out" \
  "$out/.config" "$here/uml.config" >"$out/merge.log" 2>&1)
make -C "$src" ARCH=um O="$out" -j"$(nproc)" linux
ls -l "$out/linux"
