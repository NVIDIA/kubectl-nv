/*
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package mustgather

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// DefaultArtifactDir names the directory a gather writes to when the user did
// not choose one. It carries the gpu-operator script's prefix, so a bundle
// collected by either tool is recognisable by name.
//
// The script's name is minute-resolution. Two gathers started in the same
// minute, which is what a support engineer does when the first one came out
// wrong, then land in the same directory and the second run's bundle holds the
// first run's files wherever a collector was skipped this time. The seconds and
// the suffix make each run its own.
func DefaultArtifactDir(now time.Time) string {
	return fmt.Sprintf("/tmp/nvidia-gpu-operator_%s_%s", now.Format("20060102_150405"), randomSuffix())
}

// randomSuffix returns six characters that tell two runs apart. It is not a
// secret and nothing depends on it being unguessable; a failure to read the
// system source falls back to the clock, because a name is better than no name.
func randomSuffix() string {
	var raw [3]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(raw[:])
}

// checkArtifactDir refuses a directory that already holds something.
//
// Mixing two runs produces a bundle nothing can take apart afterwards: the
// manifest indexes both runs' files and its digests check out, so the bundle
// looks consistent and describes a cluster that never existed at one moment.
// The refusal leaves what is there alone; deleting someone else's files to make
// room is a worse failure than the one being prevented.
func checkArtifactDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("reading the artifact directory: %w", err)
	case len(entries) == 0:
		return nil
	}

	return output.NewUsageError(
		"%s is not empty: refusing to mix this bundle with what is already there", dir)
}

// usableInAName reports whether a name from the cluster may be used to build a
// file name.
//
// Every pod and node name a cluster accepted is a DNS-1123 subdomain, so one
// that is not has either been tampered with or come from a confused reader.
// Such a name would reach the filesystem through a path join, and a bundle is
// unpacked by whoever receives it.
func usableInAName(name string) bool {
	return name != "" && len(validation.IsDNS1123Subdomain(name)) == 0
}
